package sops

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"sigs.k8s.io/kustomize/api/hasher"
	"sigs.k8s.io/kustomize/api/resmap"
	"sigs.k8s.io/kustomize/api/resource"
)

func TestDecryptDocumentPreservesUnnamedDocumentAndDecryptErrors(t *testing.T) {
	original := decryptFunc
	t.Cleanup(func() { decryptFunc = original })
	encrypted := []byte("apiVersion: v1\nkind: Secret\nmetadata:\n  name: \"\"\n  generateName: child-\ndata:\n  key: ENC[encrypted]\nsops: {mac: encrypted}\n")
	decrypted := []byte("apiVersion: v1\nkind: Secret\nmetadata:\n  name: \"\"\n  generateName: child-\ndata:\n  key: cGxhaW4=\n")
	decryptFunc = func(data []byte) ([]byte, error) {
		if !bytes.Equal(data, encrypted) {
			t.Fatalf("decryptor received modified identity or document: %s", data)
		}
		return decrypted, nil
	}
	got, err := DecryptDocument(encrypted)
	if err != nil || !bytes.Equal(got, decrypted) {
		t.Fatalf("unnamed decryption changed the returned document: %s, %v", got, err)
	}
	failure := errors.New("decryption key unavailable")
	decryptFunc = func([]byte) ([]byte, error) { return nil, failure }
	got, err = DecryptDocument(encrypted)
	if !errors.Is(err, failure) || len(got) != 0 {
		t.Fatalf("decryption failure exposed a document or lost its cause: %s, %v", got, err)
	}
}

func TestDecryptResourcesFailurePreservesEncryptedInventory(t *testing.T) {
	original := decryptFunc
	t.Cleanup(func() { decryptFunc = original })
	failure := errors.New("invalid ciphertext")
	decryptFunc = func([]byte) ([]byte, error) { return nil, failure }
	rm := makeSOPSResMap(t, "apiVersion: v1\nkind: Secret\nmetadata:\n  name: protected\ndata:\n  key: ENC[encrypted]\nsops: {mac: encrypted}\n")
	before, err := rm.Resources()[0].Map()
	if err != nil {
		t.Fatal(err)
	}
	if err := DecryptResources(rm); !errors.Is(err, failure) {
		t.Fatalf("decryption error lost cause: %v", err)
	}
	if rm.Size() != 1 {
		t.Fatalf("failed decryption changed inventory size: %d", rm.Size())
	}
	after, err := rm.Resources()[0].Map()
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("failed decryption replaced the encrypted resource: %+v, %v", after, err)
	}
}

func TestIsSOPSContainer(t *testing.T) {
	tests := []struct {
		name string
		m    map[string]any
		want bool
	}{
		{
			name: "has sops key",
			m: map[string]any{
				"apiVersion": "v1",
				"kind":       "Secret",
				"data":       map[string]any{"key": "val"},
				"sops":       map[string]any{"mac": "abc"},
			},
			want: true,
		},
		{
			name: "no sops key",
			m: map[string]any{
				"apiVersion": "v1",
				"kind":       "Secret",
				"data":       map[string]any{"key": "val"},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSOPSContainer(tt.m); got != tt.want {
				t.Errorf("IsSOPSContainer() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHasSOPSResources(t *testing.T) {
	rm := makeSOPSResMap(t, `apiVersion: v1
kind: Secret
metadata:
  name: sops-secret
  namespace: default
data:
  key: QUJD
sops:
  mac: "encrypted-mac"
  lastmodified: "2024-01-01T00:00:00Z"
`)

	if !HasSOPSResources(rm) {
		t.Error("expected HasSOPSResources to return true")
	}

	ids := SOPSResourceIDs(rm)
	if len(ids) != 1 {
		t.Fatalf("expected 1 SOPS resource ID, got %d", len(ids))
	}
	if ids[0].Name != "sops-secret" {
		t.Errorf("expected resource name 'sops-secret', got %q", ids[0].Name)
	}
}

func TestHasSOPSResources_NoSOPS(t *testing.T) {
	rm := makeResMap(t, `apiVersion: v1
kind: Secret
metadata:
  name: plain-secret
  namespace: default
data:
  key: QUJD
`)

	if HasSOPSResources(rm) {
		t.Error("expected HasSOPSResources to return false for plain secret")
	}
}

func makeResMap(t *testing.T, y string) resmap.ResMap {
	t.Helper()
	factory := resmap.NewFactory(resource.NewFactory(&hasher.Hasher{}))
	rm, err := factory.NewResMapFromBytes([]byte(y))
	if err != nil {
		t.Fatalf("failed to create resmap: %v", err)
	}
	return rm
}

func makeSOPSResMap(t *testing.T, y string) resmap.ResMap {
	t.Helper()
	return makeResMap(t, y)
}

func TestDecryptResources_ReplacesEncryptedSecret(t *testing.T) {
	origDecrypt := decryptFunc
	defer func() { decryptFunc = origDecrypt }()

	decryptedYAML := []byte(`apiVersion: v1
kind: Secret
metadata:
  name: sops-secret
  namespace: default
data:
  key: REVMT0M=
`)
	decryptFunc = func(data []byte) ([]byte, error) {
		return decryptedYAML, nil
	}

	rm := makeSOPSResMap(t, `apiVersion: v1
kind: Secret
metadata:
  name: sops-secret
  namespace: default
data:
  key: QUJD
sops:
  mac: "encrypted-mac"
`)

	if err := DecryptResources(rm); err != nil {
		t.Fatalf("DecryptResources() error = %v", err)
	}

	res := rm.Resources()
	if len(res) != 1 {
		t.Fatalf("expected 1 resource, got %d", len(res))
	}
	m, err := res[0].Map()
	if err != nil {
		t.Fatalf("Map() error = %v", err)
	}
	if _, hasSOPS := m["sops"]; hasSOPS {
		t.Error("expected sops key to be removed after decryption")
	}
}

func TestDecryptResources_SkipsNonSOPSSecrets(t *testing.T) {
	origDecrypt := decryptFunc
	defer func() { decryptFunc = origDecrypt }()

	decryptFunc = func(data []byte) ([]byte, error) {
		t.Fatal("decryptFunc should not be called for non-SOPS secrets")
		return nil, nil
	}

	rm := makeResMap(t, `apiVersion: v1
kind: Secret
metadata:
  name: plain-secret
  namespace: default
data:
  key: QUJD
`)

	if err := DecryptResources(rm); err != nil {
		t.Fatalf("DecryptResources() error = %v", err)
	}
}

func TestDecryptResources_SkipsNonSecrets(t *testing.T) {
	origDecrypt := decryptFunc
	defer func() { decryptFunc = origDecrypt }()

	decryptFunc = func(data []byte) ([]byte, error) {
		t.Fatal("decryptFunc should not be called for non-Secret resources")
		return nil, nil
	}

	rm := makeResMap(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: test-cm
  namespace: default
data:
  key: value
`)

	if err := DecryptResources(rm); err != nil {
		t.Fatalf("DecryptResources() error = %v", err)
	}
}
