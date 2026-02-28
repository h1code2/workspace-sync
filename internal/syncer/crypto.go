package syncer

import (
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

type wireMessage struct {
	Nonce string `json:"nonce"`
	Data  string `json:"data"`
}

type fileEvent struct {
	Type     string `json:"type"` // upsert | delete | snapshot_begin | snapshot_end
	Snapshot string `json:"snapshot,omitempty"`
	Path     string `json:"path,omitempty"`
	Mode     uint32 `json:"mode,omitempty"`
	ModTime  int64  `json:"mod_time,omitempty"`
	Data     []byte `json:"data,omitempty"`
}

func deriveKey(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func sign(token string, challenge []byte) []byte {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write(challenge)
	return mac.Sum(nil)
}

func encrypt(token string, evt fileEvent) ([]byte, error) {
	plain, err := json.Marshal(evt)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(plain); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	plain = buf.Bytes()

	block, err := aes.NewCipher(deriveKey(token))
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	ciphertext := gcm.Seal(nil, nonce, plain, nil)
	wm := wireMessage{
		Nonce: base64.StdEncoding.EncodeToString(nonce),
		Data:  base64.StdEncoding.EncodeToString(ciphertext),
	}
	return json.Marshal(wm)
}

func decrypt(token string, packet []byte) (fileEvent, error) {
	var wm wireMessage
	if err := json.Unmarshal(packet, &wm); err != nil {
		return fileEvent{}, err
	}
	nonce, err := base64.StdEncoding.DecodeString(wm.Nonce)
	if err != nil {
		return fileEvent{}, err
	}
	ciphertext, err := base64.StdEncoding.DecodeString(wm.Data)
	if err != nil {
		return fileEvent{}, err
	}

	block, err := aes.NewCipher(deriveKey(token))
	if err != nil {
		return fileEvent{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return fileEvent{}, err
	}
	if len(nonce) != gcm.NonceSize() {
		return fileEvent{}, fmt.Errorf("invalid nonce size")
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return fileEvent{}, err
	}

	zr, err := gzip.NewReader(bytes.NewReader(plain))
	if err != nil {
		return fileEvent{}, err
	}
	defer zr.Close()
	decoded, err := io.ReadAll(zr)
	if err != nil {
		return fileEvent{}, err
	}

	var evt fileEvent
	if err := json.Unmarshal(decoded, &evt); err != nil {
		return fileEvent{}, err
	}
	switch evt.Type {
	case "upsert", "delete":
		if evt.Path == "" {
			return fileEvent{}, errors.New("empty path")
		}
	case "snapshot_begin", "snapshot_end":
		if evt.Snapshot == "" {
			return fileEvent{}, errors.New("empty snapshot id")
		}
	default:
		return fileEvent{}, errors.New("invalid type")
	}
	return evt, nil
}
