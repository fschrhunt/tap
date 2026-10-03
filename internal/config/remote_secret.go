package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func remoteSecretsPath(path string) string { return path + ".remote-secrets.json" }

func loadRemoteSecrets(path string) (map[string]string, error) {
	secrets := map[string]string{}
	file := remoteSecretsPath(path)
	info, err := os.Lstat(file)
	if os.IsNotExist(err) {
		return secrets, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("remote credentials must be in an owner-only regular file")
	}
	b, err := os.ReadFile(file)
	if err != nil || json.Unmarshal(b, &secrets) != nil {
		return nil, fmt.Errorf("cannot read remote credentials")
	}
	return secrets, nil
}

// LoadRemoteSecret reads one paired-device token from its owner-only sidecar.
func LoadRemoteSecret(path, id string) (string, error) {
	secrets, err := loadRemoteSecrets(path)
	if err != nil {
		return "", err
	}
	return secrets[id], nil
}

// SaveRemoteSecret keeps paired-device tokens outside the shareable server config.
func SaveRemoteSecret(path, id, token string) error {
	return editRemoteSecrets(path, func(secrets map[string]string) error {
		secrets[id] = token
		return nil
	})
}

// RemoveRemoteSecret forgets one paired-device token.
func RemoveRemoteSecret(path, id string) error {
	return editRemoteSecrets(path, func(secrets map[string]string) error {
		delete(secrets, id)
		return nil
	})
}

func editRemoteSecrets(path string, change func(map[string]string) error) error {
	file := remoteSecretsPath(path)
	if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
		return fmt.Errorf("cannot create remote credential directory")
	}
	lock, err := os.OpenFile(file+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("cannot lock remote credentials")
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("cannot lock remote credentials")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	secrets, err := loadRemoteSecrets(path)
	if err != nil {
		return err
	}
	if err = change(secrets); err != nil {
		return err
	}
	b, err := json.Marshal(secrets)
	if err != nil {
		return fmt.Errorf("cannot encode remote credentials")
	}
	temp, err := os.CreateTemp(filepath.Dir(file), ".tap-remote-*.tmp")
	if err != nil {
		return fmt.Errorf("cannot write remote credentials")
	}
	defer os.Remove(temp.Name())
	if err := temp.Chmod(0600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("cannot protect remote credentials")
	}
	if _, err = temp.Write(append(b, '\n')); err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err != nil || closeErr != nil {
		return fmt.Errorf("cannot write remote credentials")
	}
	if err = os.Rename(temp.Name(), file); err != nil {
		return fmt.Errorf("cannot save remote credentials")
	}
	if dir, err := os.Open(filepath.Dir(file)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
