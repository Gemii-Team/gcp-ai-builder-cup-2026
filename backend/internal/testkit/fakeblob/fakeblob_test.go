package fakeblob_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"testing"
	"time"

	"kiwi/internal/platform/blob"
	"kiwi/internal/testkit/fakeblob"
)

func TestStore_PutHeadReadDelete(t *testing.T) {
	ctx := context.Background()
	s := fakeblob.New()
	data := []byte("hello")
	s.Put("uploads/a", "text/plain", data)
	data[0] = 'X' // the store must have copied it

	attrs, err := s.Head(ctx, "uploads/a")
	if err != nil || attrs != (blob.Attrs{Size: 5, ContentType: "text/plain"}) {
		t.Fatalf("Head = (%+v, %v), want (5 bytes text/plain, nil)", attrs, err)
	}
	r, err := s.Read(ctx, "uploads/a")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	got, _ := io.ReadAll(r)
	_ = r.Close()
	if string(got) != "hello" {
		t.Fatalf("Read = %q, want hello", got)
	}

	if err := s.Delete(ctx, "uploads/a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete(ctx, "uploads/a"); err != nil {
		t.Fatalf("Delete of a missing key = %v, want nil", err)
	}
}

func TestStore_MissingKeyWrapsErrNotExist(t *testing.T) {
	ctx := context.Background()
	s := fakeblob.New()
	if _, err := s.Head(ctx, "nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Head error = %v, want fs.ErrNotExist", err)
	}
	if _, err := s.Read(ctx, "nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Read error = %v, want fs.ErrNotExist", err)
	}
}

func TestStore_SignedURLsAreRecorded(t *testing.T) {
	ctx := context.Background()
	s := fakeblob.New()

	url, headers, err := s.SignedPut(ctx, "uploads/a b", "application/pdf", 1<<20, time.Minute)
	if err != nil || url == "" || headers["Content-Type"] != "application/pdf" {
		t.Fatalf("SignedPut = (%q, %v, %v)", url, headers, err)
	}
	if got, err := s.SignedGet(ctx, "uploads/a b", 5*time.Minute); err != nil || got == "" || got == url {
		t.Fatalf("SignedGet = (%q, %v), want a URL different from the PUT URL", got, err)
	}
	if s.URI("uploads/a b") != "" {
		t.Fatal("URI must be empty, as on the local file store")
	}

	want := []fakeblob.Grant{
		{Method: "PUT", Key: "uploads/a b", ContentType: "application/pdf", MaxBytes: 1 << 20, TTL: time.Minute},
		{Method: "GET", Key: "uploads/a b", TTL: 5 * time.Minute},
	}
	got := s.Grants()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Grants = %+v, want %+v", got, want)
	}
}
