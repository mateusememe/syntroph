package storage_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	"github.com/mateusememe/syntroph/storage"
	"github.com/mateusememe/syntroph/storage/contracttest"
)

func TestDeterministicFakeStoragePortContract(t *testing.T) {
	contracttest.Run(t, "deterministic-fake", func(t *testing.T, root string) contracttest.Fixture {
		client := &contractClient{documents: map[string]storage.RemoteDocument{}}
		remote, err := storage.NewMirror(storage.BackendIssues, "mateusememe/syntroph", client)
		if err != nil {
			t.Fatal(err)
		}
		managed, err := storage.NewManagedMirror(root, "deterministic-fake", remote)
		if err != nil {
			t.Fatal(err)
		}
		return contracttest.Fixture{
			Port: managed, Bindings: managed.Bindings, Backend: storage.BackendIssues, Provider: "deterministic-fake",
			CreateCount: func() int { client.mu.Lock(); defer client.mu.Unlock(); return client.creates },
			MutateRemote: func(t *testing.T, created storage.MirrorResult, content string) {
				client.mu.Lock()
				defer client.mu.Unlock()
				doc := client.documents[created.Key]
				doc.Content, doc.Revision = content, contractIssueRevision(doc.ID, content)
				client.documents[created.Key] = doc
			},
		}
	})
}

type contractClient struct {
	mu        sync.Mutex
	documents map[string]storage.RemoteDocument
	creates   int
}

func (c *contractClient) Get(_ context.Context, _ storage.Backend, _, key string) (storage.RemoteDocument, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	doc, ok := c.documents[key]
	if !ok {
		return storage.RemoteDocument{NotFound: true}, nil
	}
	return doc, nil
}

func (c *contractClient) Put(_ context.Context, _ storage.Backend, _, key, content, _ string) (storage.RemoteDocument, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.creates++
	id := "42"
	if existing, ok := c.documents[key]; ok {
		id = existing.ID
	}
	doc := storage.RemoteDocument{ID: id, URL: "https://example.test/issues/" + id, Revision: contractIssueRevision(id, content), Content: content}
	c.documents[key] = doc
	return doc, nil
}

func contractIssueRevision(id, content string) string {
	sum := sha256.Sum256([]byte(content))
	return "issue:" + id + ":" + time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano) + ":" + hex.EncodeToString(sum[:])
}
