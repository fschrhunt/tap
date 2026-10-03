// Package discovery ranks MCP capabilities locally without network calls or models.
// Names, providers, documentation and parameter descriptions have distinct weights.
package discovery

import (
	"math"
	"runtime"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/fschrhunt/tap/internal/wire"
)

// Tool is an immutable catalog record; ID is the exact downstream routing address.
type Tool struct {
	ID, Server string
	Definition wire.Object
}

// Match associates a catalog record with a retrieval score, not a probability.
type Match struct {
	Tool  Tool
	Score float64
}

// Query and candidate limits bound ranking memory and repeated catalog scans.
const MaxQueryBytes = 4096
const MaxQueryTerms = 32
const MaxRankTools = 65536

var stops = map[string]bool{"a": true, "an": true, "the": true, "to": true, "of": true, "for": true, "in": true, "on": true, "with": true, "my": true, "me": true, "please": true, "can": true, "you": true, "i": true, "want": true, "would": true, "like": true, "and": true, "is": true}

// scan calls word with each lowercase word of s in turn, splitting identifiers and Unicode
// text into words rather than substrings. A capital after an ASCII lowercase letter or digit
// starts a new word. The bytes passed are only valid during the call.
func scan(s string, word func([]byte)) {
	var buf [64]byte
	w := buf[:0]
	var prev rune
	for _, r := range s {
		if (prev >= 'a' && prev <= 'z' || prev >= '0' && prev <= '9') && r >= 'A' && r <= 'Z' && len(w) > 0 {
			word(w)
			w = w[:0]
		}
		prev = r
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			w = append(w, byte(r))
		case r >= 'A' && r <= 'Z':
			w = append(w, byte(r)+'a'-'A')
		case r < utf8.RuneSelf:
			if len(w) > 0 {
				word(w)
				w = w[:0]
			}
		default:
			if lower := unicode.ToLower(r); unicode.IsLetter(lower) || unicode.IsDigit(lower) {
				w = utf8.AppendRune(w, lower)
			} else if len(w) > 0 {
				word(w)
				w = w[:0]
			}
		}
	}
	if len(w) > 0 {
		word(w)
	}
}

// Tokens returns the words of s, as scan finds them.
func Tokens(s string) []string {
	out := []string{}
	scan(s, func(w []byte) { out = append(out, string(w)) })
	return out
}

// term turns a word into the form it is indexed and asked under: nil for query filler, and
// English plurals conservatively made singular. It may shorten or change w in place.
func term(w []byte) []byte {
	if stops[string(w)] {
		return nil
	}
	if n := len(w); n > 4 && string(w[n-3:]) == "ies" {
		w = append(w[:n-3], 'y')
	} else if n > 3 && w[n-1] == 's' && w[n-2] != 's' {
		w = w[:n-1]
	}
	return w
}

// terms removes filler, normalizes plurals and deduplicates at most 32 query terms.
func terms(s string) []string {
	out := []string{}
	seen := map[string]bool{}
	scan(s, func(w []byte) {
		if w = term(w); w != nil && !seen[string(w)] && len(out) < MaxQueryTerms {
			seen[string(w)] = true
			out = append(out, string(w))
		}
	})
	return out
}

// schemaWords scans parameter names, descriptions and enum labels, not structural noise.
func schemaWords(v any, word func([]byte)) {
	switch x := v.(type) {
	case wire.Object:
		for _, f := range x {
			switch f.Name {
			case "description", "title", "enum":
				scan(wire.String(f.Value), word)
			case "properties":
				if o, ok := f.Value.(wire.Object); ok {
					for _, p := range o {
						scan(p.Name, word)
						schemaWords(p.Value, word)
					}
				}
			default:
				schemaWords(f.Value, word)
			}
		}
	case []any:
		for _, y := range x {
			schemaWords(y, word)
		}
	}
}

// words calls word with every term of a tool and the field it stands in: 0 the id, 1 the
// server, 2 the title and description, 3 the schema. The bytes are only valid during the call.
func words(t Tool, word func(field int, w []byte)) {
	field := 0
	emit := func(w []byte) {
		if w = term(w); w != nil {
			word(field, w)
		}
	}
	scan(t.ID, emit)
	field = 1
	scan(t.Server, emit)
	field = 2
	scan(stringField(t.Definition, "title"), emit)
	scan(stringField(t.Definition, "description"), emit)
	field = 3
	schemaWords(t.Definition.Get("inputSchema"), emit)
}

// tally is what one tool holds of a query: how long each field is, and how often each field
// has each query word, at counts[4*word+field].
type tally struct {
	lengths [4]int32
	counts  []int32
}

// count reads every tool once for the words of q, on several processors at once.
func count(tools []Tool, q []string) []tally {
	docs := make([]tally, len(tools))
	counts := make([]int32, 4*len(q)*len(tools))
	workers := min(runtime.GOMAXPROCS(0), 8, len(tools)/16+1)
	var wg sync.WaitGroup
	for part := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := part; i < len(tools); i += workers {
				d := &docs[i]
				d.counts = counts[4*len(q)*i : 4*len(q)*(i+1)]
				words(tools[i], func(f int, w []byte) {
					d.lengths[f]++
					for j, word := range q {
						if string(w) == word {
							d.counts[4*j+f]++
						}
					}
				})
			}
		}()
	}
	wg.Wait()
	return docs
}

// Rank performs field-weighted BM25 with stable catalog-order ties. A query that is a tool's
// name or id, word for word, returns the tools so named. Otherwise the tools that hold
// all of the first 32 distinct normalized terms are returned, and when none does,
// the tools that hold any: unknown vocabulary does not eliminate relevant tools,
// and zero overlap returns none.
//
// Nothing is kept between queries: each one reads the catalog it is given, which costs less
// than a millisecond for hundreds of tools and leaves nothing to build before the first.
// Queries over 4096 bytes or catalogs over 65536 tools return no matches; gateways
// must reject these inputs with a diagnostic before invoking Rank.
func Rank(tools []Tool, query string) []Match {
	if len(query) > MaxQueryBytes || len(tools) > MaxRankTools {
		return nil
	}
	if asked := strings.Join(Tokens(query), " "); asked != "" {
		named := []Match{}
		for _, t := range tools {
			_, name, _ := strings.Cut(t.ID, ".")
			if strings.Join(Tokens(t.ID), " ") == asked {
				named = append(named, Match{t, 2000})
			} else if name != "" && strings.Join(Tokens(name), " ") == asked {
				named = append(named, Match{t, 1000})
			}
		}
		if len(named) > 0 {
			sort.SliceStable(named, func(i, j int) bool { return named[i].Score > named[j].Score })
			return named
		}
	}
	q := terms(query)
	if len(q) == 0 {
		return nil
	}
	docs := count(tools, q)
	df := func(j int) (n int) {
		for _, d := range docs {
			if c := d.counts[4*j : 4*j+4]; c[0]+c[1]+c[2]+c[3] > 0 {
				n++
			}
		}
		return n
	}
	// Correct only unique, one-edit out-of-vocabulary words; never widen known terms.
	corrected := false
	for j, word := range q {
		if len([]rune(word)) < 5 || df(j) > 0 {
			continue
		}
		candidate, ambiguous := "", false
		for _, t := range tools {
			words(t, func(_ int, w []byte) {
				// One edit leaves the first or the last letter in place.
				if (w[0] == word[0] || w[len(w)-1] == word[len(word)-1]) && string(w) != candidate && oneEdit(word, string(w)) {
					ambiguous = ambiguous || candidate != ""
					candidate = string(w)
				}
			})
		}
		if candidate != "" && !ambiguous {
			q[j], corrected = candidate, true
		}
	}
	if corrected {
		docs = count(tools, q)
	}
	var avg [4]float64
	for _, d := range docs {
		for f := range avg {
			avg[f] += float64(d.lengths[f])
		}
	}
	for f := range avg {
		if len(docs) > 0 {
			avg[f] = math.Max(1, avg[f]/float64(len(docs)))
		}
	}
	weights := [4]float64{5, 8, 2, 1}
	idfs := make([]float64, len(q))
	for j := range q {
		n := float64(df(j))
		idfs[j] = math.Log(1 + (float64(len(docs))-n+0.5)/(n+0.5))
	}
	out, whole := []Match{}, []Match{}
	for i, d := range docs {
		score, covered := 0.0, 0
		for j := range q {
			found := false
			for f, n := range d.counts[4*j : 4*j+4] {
				tf := float64(n)
				if tf == 0 {
					continue
				}
				found = true
				score += weights[f] * idfs[j] * tf * 2.2 / (tf + 1.2*(0.25+0.75*float64(d.lengths[f])/avg[f]))
			}
			if found {
				covered++
			}
		}
		if score > 0 {
			m := Match{tools[i], score * (1 + float64(covered)/float64(len(q)))}
			out = append(out, m)
			if covered == len(q) {
				whole = append(whole, m)
			}
		}
	}
	if len(whole) > 0 {
		out = whole
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// oneEdit recognizes one insertion, deletion or substitution without fuzzy substring matches.
func oneEdit(a, b string) bool {
	// Reject large length differences before allocating rune slices for server text.
	na, nb := utf8.RuneCountInString(a), utf8.RuneCountInString(b)
	if na-nb > 1 || nb-na > 1 {
		return false
	}
	x, y := []rune(a), []rune(b)
	if len(x) > len(y) {
		x, y = y, x
	}
	if len(y)-len(x) > 1 {
		return false
	}
	i, j, edits := 0, 0, 0
	for i < len(x) && j < len(y) {
		if x[i] == y[j] {
			i++
			j++
			continue
		}
		edits++
		if edits > 1 {
			return false
		}
		if len(x) == len(y) {
			i++
		}
		j++
	}
	if j < len(y) {
		edits++
	}
	return edits == 1
}

// stringField reads optional textual metadata without coercing arbitrary JSON.
func stringField(o wire.Object, key string) string { s, _ := o.Get(key).(string); return s }
