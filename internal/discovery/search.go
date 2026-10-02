// Package discovery ranks MCP capabilities locally without network calls or models.
// Names, providers, documentation and parameter descriptions have distinct weights.
package discovery

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"

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

var camel = regexp.MustCompile(`([a-z0-9])([A-Z])`)
var stops = map[string]bool{"a": true, "an": true, "the": true, "to": true, "of": true, "for": true, "in": true, "on": true, "with": true, "my": true, "me": true, "please": true, "can": true, "you": true, "i": true, "want": true, "would": true, "like": true, "and": true, "is": true}

// Tokens splits identifiers and Unicode text, preserving words rather than substrings.
func Tokens(s string) []string {
	s = strings.ToLower(camel.ReplaceAllString(s, "$1 $2"))
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// terms removes query filler and conservatively normalizes English plurals.
func terms(s string) []string {
	out := []string{}
	for _, t := range Tokens(s) {
		if stops[t] {
			continue
		}
		if len(t) > 4 && strings.HasSuffix(t, "ies") {
			t = t[:len(t)-3] + "y"
		} else if len(t) > 3 && strings.HasSuffix(t, "s") && !strings.HasSuffix(t, "ss") {
			t = t[:len(t)-1]
		}
		out = append(out, t)
	}
	return out
}

// schemaText indexes parameter names, descriptions and enum labels, not structural noise.
func schemaText(v any) string {
	var b strings.Builder
	var visit func(any)
	visit = func(v any) {
		switch x := v.(type) {
		case wire.Object:
			for _, f := range x {
				switch f.Name {
				case "description", "title", "enum":
					b.WriteString(wire.String(f.Value))
					b.WriteByte(' ')
				case "properties":
					if o, ok := f.Value.(wire.Object); ok {
						for _, p := range o {
							b.WriteString(p.Name)
							b.WriteByte(' ')
							visit(p.Value)
						}
					}
				default:
					visit(f.Value)
				}
			}
		case []any:
			for _, y := range x {
				visit(y)
			}
		}
	}
	visit(v)
	return b.String()
}

// Index is an immutable metadata index, safe for concurrent queries until catalogs change.
type Index struct {
	tools []Tool
	docs  []document
	df    map[string]int
	avg   [4]float64
}
type document struct {
	fields  [4]map[string]int
	lengths [4]int
}

// New indexes a catalog once; callers must not mutate its definitions afterward.
func New(tools []Tool) *Index {
	type doc = document
	docs := make([]doc, len(tools))
	df := map[string]int{}
	var avg [4]float64
	for i, t := range tools {
		texts := [4]string{t.ID, t.Server, stringField(t.Definition, "title") + " " + stringField(t.Definition, "description"), schemaText(t.Definition.Get("inputSchema"))}
		seen := map[string]bool{}
		for f, text := range texts {
			docs[i].fields[f] = map[string]int{}
			for _, term := range terms(text) {
				docs[i].fields[f][term]++
				docs[i].lengths[f]++
				seen[term] = true
			}
			avg[f] += float64(docs[i].lengths[f])
		}
		for term := range seen {
			df[term]++
		}
	}
	for f := range avg {
		if len(tools) > 0 {
			avg[f] = math.Max(1, avg[f]/float64(len(tools)))
		}
	}
	return &Index{tools: tools, docs: docs, df: df, avg: avg}
}

// Rank indexes and ranks a frozen catalog in one call, useful for cold retrieval evaluation.
func Rank(tools []Tool, query string) []Match { return New(tools).Search(query) }

// Search performs field-weighted BM25 with coverage preference and stable catalog-order ties.
// Unknown vocabulary does not eliminate relevant tools; zero overlap returns none.
func (index *Index) Search(query string) []Match {
	q := terms(query)
	if len(q) == 0 {
		return nil
	}
	tools, docs, df, avg := index.tools, index.docs, index.df, index.avg
	// Correct only unique, one-edit out-of-vocabulary words; never widen known terms.
	for i, term := range q {
		if df[term] > 0 || len([]rune(term)) < 5 {
			continue
		}
		candidate := ""
		ambiguous := false
		for word := range df {
			if oneEdit(term, word) {
				if candidate != "" {
					ambiguous = true
					break
				}
				candidate = word
			}
		}
		if candidate != "" && !ambiguous {
			q[i] = candidate
		}
	}
	weights := [4]float64{5, 8, 2, 1}
	idfs := make([]float64, len(q))
	for i, term := range q {
		idfs[i] = math.Log(1 + (float64(len(tools)-df[term])+0.5)/(float64(df[term])+0.5))
	}
	out := []Match{}
	for i, t := range tools {
		score, covered := 0.0, 0
		for j, term := range q {
			found := false
			idf := idfs[j]
			for f, counts := range docs[i].fields {
				tf := float64(counts[term])
				if tf == 0 {
					continue
				}
				found = true
				score += weights[f] * idf * tf * 2.2 / (tf + 1.2*(0.25+0.75*float64(docs[i].lengths[f])/avg[f]))
			}
			if found {
				covered++
			}
		}
		if strings.EqualFold(strings.TrimSpace(query), t.ID) {
			score += 1000
		}
		if score > 0 {
			out = append(out, Match{t, score * (1 + float64(covered)/float64(len(q)))})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// oneEdit recognizes one insertion, deletion or substitution without fuzzy substring matches.
func oneEdit(a, b string) bool {
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
