package remote

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

const pairingService = "_tap._tcp"

type deviceRecord struct {
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	TokenHash string    `json:"tokenHash"`
	AddedAt   time.Time `json:"addedAt"`
}

type deviceRegistry struct {
	Devices map[string]deviceRecord `json:"devices"`
}

// Device is the safe, printable information about one paired client.
type Device struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Role    string    `json:"role"`
	AddedAt time.Time `json:"addedAt"`
}

// deviceManager pins the host identity and issues revocable per-device credentials.
type deviceManager struct {
	path        string
	cert        tls.Certificate
	id          string
	fingerprint string
	name        string
	adminCode   string
	execCode    string
	expires     time.Time
	mu          sync.Mutex
	registry    deviceRegistry
	badAttempts int
	adminUsed   bool
	execUsed    bool
}

func newDeviceManager(path, certFile, keyFile string) (*deviceManager, error) {
	var cert tls.Certificate
	var err error
	if certFile != "" {
		cert, err = tls.LoadX509KeyPair(certFile, keyFile)
	} else {
		cert, err = loadOrCreateIdentity(path)
	}
	if err != nil {
		return nil, fmt.Errorf("cannot load remote device identity")
	}
	if len(cert.Certificate) == 0 {
		return nil, fmt.Errorf("remote identity has no certificate")
	}
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("invalid remote device identity")
	}
	fingerprint := sha256.Sum256(parsed.Raw)
	host, _ := os.Hostname()
	if host == "" {
		host = "tap"
	}
	m := &deviceManager{path: path, cert: cert, fingerprint: hex.EncodeToString(fingerprint[:]), id: hex.EncodeToString(fingerprint[:8]), name: host,
		expires: time.Now().Add(15 * time.Minute)}
	if m.adminCode, err = pairingCode(); err != nil {
		return nil, fmt.Errorf("cannot prepare remote pairing")
	}
	if m.execCode, err = pairingCode(); err != nil {
		return nil, fmt.Errorf("cannot prepare remote pairing")
	}
	if err = m.reload(); err != nil {
		return nil, err
	}
	return m, nil
}

func pairingCode() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func loadOrCreateIdentity(path string) (tls.Certificate, error) {
	certPath, keyPath := path+".device.crt", path+".device.key"
	for _, file := range []string{certPath, keyPath} {
		if info, err := os.Lstat(file); err == nil {
			if !info.Mode().IsRegular() || (file == keyPath && info.Mode().Perm()&0077 != 0) {
				return tls.Certificate{}, fmt.Errorf("device identity files must be regular and the private key owner-only")
			}
		} else if !os.IsNotExist(err) {
			return tls.Certificate{}, fmt.Errorf("cannot inspect device identity")
		}
	}
	cert, certErr := os.ReadFile(certPath)
	key, keyErr := os.ReadFile(keyPath)
	if certErr == nil && keyErr == nil {
		return tls.X509KeyPair(cert, key)
	}
	if !os.IsNotExist(certErr) || !os.IsNotExist(keyErr) {
		return tls.Certificate{}, fmt.Errorf("cannot read device identity")
	}
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serialBytes := make([]byte, 16)
	if _, err := rand.Read(serialBytes); err != nil {
		return tls.Certificate{}, err
	}
	serial := new(big.Int).SetBytes(serialBytes)
	template := x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "tap remote"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().AddDate(10, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, pub, private)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
	if err = os.MkdirAll(filepath.Dir(certPath), 0700); err != nil {
		return tls.Certificate{}, err
	}
	if err = writeExclusive(certPath, certPEM); err != nil {
		return tls.Certificate{}, err
	}
	if err = writeExclusive(keyPath, keyPEM); err != nil {
		_ = os.Remove(certPath)
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

func writeExclusive(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	return closeErr
}

func (m *deviceManager) registryPath() string { return m.path + ".devices.json" }

func (m *deviceManager) reload() error {
	file := m.registryPath()
	info, err := os.Lstat(file)
	if os.IsNotExist(err) {
		m.registry = deviceRegistry{Devices: map[string]deviceRecord{}}
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("remote device registry must be an owner-only regular file")
	}
	b, err := os.ReadFile(file)
	var registry deviceRegistry
	if err != nil || json.Unmarshal(b, &registry) != nil || registry.Devices == nil {
		return fmt.Errorf("cannot read remote device registry")
	}
	m.registry = registry
	return nil
}

func (m *deviceManager) role(token string) string {
	if token == "" {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reload() != nil {
		return ""
	}
	hash := sha256.Sum256([]byte(token))
	encoded := hex.EncodeToString(hash[:])
	for _, device := range m.registry.Devices {
		if subtle.ConstantTimeCompare([]byte(encoded), []byte(device.TokenHash)) == 1 {
			return device.Role
		}
	}
	return ""
}

func (m *deviceManager) enroll(code, name, role string) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if time.Now().After(m.expires) || (role != "admin" && role != "execution") {
		return "", "", fmt.Errorf("pairing code expired or invalid")
	}
	want := m.execCode
	used := &m.execUsed
	if role == "admin" {
		want = m.adminCode
		used = &m.adminUsed
	}
	if *used {
		return "", "", fmt.Errorf("pairing code was already used")
	}
	if subtle.ConstantTimeCompare([]byte(code), []byte(want)) != 1 {
		m.badAttempts++
		return "", "", fmt.Errorf("pairing code is incorrect")
	}
	if m.badAttempts >= 20 || len(m.registry.Devices) >= 64 {
		return "", "", fmt.Errorf("pairing is temporarily unavailable")
	}
	*used = true
	for _, char := range name {
		if char < 0x20 || char == 0x7f {
			return "", "", fmt.Errorf("invalid device name")
		}
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 80 {
		return "", "", fmt.Errorf("device name must be 1 to 80 characters")
	}
	idBytes := make([]byte, 9)
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(idBytes); err != nil {
		return "", "", err
	}
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", "", err
	}
	id := base64.RawURLEncoding.EncodeToString(idBytes)
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	hash := sha256.Sum256([]byte(token))
	var registry deviceRegistry
	err := updateRegistry(m.registryPath(), func(current *deviceRegistry) error {
		if len(current.Devices) >= 64 {
			return fmt.Errorf("remote device limit reached")
		}
		current.Devices[id] = deviceRecord{Name: name, Role: role, TokenHash: hex.EncodeToString(hash[:]), AddedAt: time.Now().UTC()}
		registry = *current
		return nil
	})
	if err != nil {
		return "", "", err
	}
	m.registry = registry
	return id, token, nil
}

func updateRegistry(path string, change func(*deviceRegistry) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("cannot protect remote device registry")
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("cannot lock remote device registry")
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("cannot lock remote device registry")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	registry := deviceRegistry{Devices: map[string]deviceRecord{}}
	if info, statErr := os.Lstat(path); statErr == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("remote device registry must be an owner-only regular file")
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil || json.Unmarshal(b, &registry) != nil || registry.Devices == nil {
			return fmt.Errorf("cannot read remote device registry")
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("cannot inspect remote device registry")
	}
	if err := change(&registry); err != nil {
		return err
	}
	b, err := json.Marshal(registry)
	if err != nil {
		return fmt.Errorf("cannot encode remote device registry")
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".tap-devices-*.tmp")
	if err != nil {
		return fmt.Errorf("cannot write remote device registry")
	}
	defer os.Remove(temp.Name())
	if err = temp.Chmod(0600); err == nil {
		_, err = temp.Write(append(b, '\n'))
	}
	if err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err != nil || closeErr != nil {
		return fmt.Errorf("cannot write remote device registry")
	}
	if err = os.Rename(temp.Name(), path); err != nil {
		return fmt.Errorf("cannot save remote device registry")
	}
	return nil
}

// PairedDevices lists clients enrolled on this serving machine without exposing credentials.
func PairedDevices(path string) ([]Device, error) {
	m := &deviceManager{path: path}
	if err := m.reload(); err != nil {
		return nil, err
	}
	out := make([]Device, 0, len(m.registry.Devices))
	for id, device := range m.registry.Devices {
		out = append(out, Device{ID: id, Name: device.Name, Role: device.Role, AddedAt: device.AddedAt})
	}
	slices.SortFunc(out, func(a, b Device) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// RevokeDevice removes a paired client's credential so its next request is rejected.
func RevokeDevice(path, id string) (bool, error) {
	revoked := false
	err := updateRegistry(path+".devices.json", func(registry *deviceRegistry) error {
		if _, ok := registry.Devices[id]; ok {
			delete(registry.Devices, id)
			revoked = true
		}
		return nil
	})
	return revoked, err
}

func (m *deviceManager) certificate() tls.Certificate { return m.cert }
