package acme

import (
	"context"
	"errors"
	"io/fs"
	"reflect"
	"testing"
	"time"

	"github.com/enetforge/hogshead/internal/meta"
)

func TestStorageRoundTrip(t *testing.T) {
	s, _, c := newStorage(t)
	ctx := context.Background()

	if _, err := s.Load(ctx, "certificates/acme/example.com/example.com.crt"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Load missing: want fs.ErrNotExist, got %v", err)
	}
	if s.Exists(ctx, "certificates") {
		t.Fatal("Exists on empty store should be false")
	}

	key := "certificates/acme/example.com/example.com.crt"
	if err := s.Store(ctx, key, []byte("pem")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(ctx, key)
	if err != nil || string(got) != "pem" {
		t.Fatalf("Load: %q %v", got, err)
	}
	// Overwrite.
	c.advance(time.Minute)
	if err := s.Store(ctx, key, []byte("pem2")); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Load(ctx, key)
	if string(got) != "pem2" {
		t.Fatalf("Load after overwrite: %q", got)
	}

	info, err := s.Stat(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsTerminal || info.Size != 4 || info.Key != key || !info.Modified.Equal(c.now()) {
		t.Fatalf("Stat: %+v", info)
	}
	for _, dir := range []string{"certificates", "certificates/", "certificates/acme/example.com"} {
		info, err = s.Stat(ctx, dir)
		if err != nil || info.IsTerminal {
			t.Fatalf("Stat dir %q: %+v %v", dir, info, err)
		}
		if !s.Exists(ctx, dir) {
			t.Fatalf("Exists dir %q: false", dir)
		}
	}
	if _, err := s.Stat(ctx, "nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Stat missing: %v", err)
	}
	if _, err := s.Stat(ctx, ""); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Stat empty key: %v", err)
	}

	if err := s.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if s.Exists(ctx, key) {
		t.Fatal("Exists after Delete")
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("Delete missing should be nil, got %v", err)
	}
}

func TestStorageStoreEmptyKey(t *testing.T) {
	s, _, _ := newStorage(t)
	if err := s.Store(context.Background(), "", []byte("x")); err == nil {
		t.Fatal("want error for empty key")
	}
}

func TestStorageList(t *testing.T) {
	s, _, _ := newStorage(t)
	ctx := context.Background()
	for _, k := range []string{
		"certificates/acme/a.com/a.com.crt",
		"certificates/acme/a.com/a.com.key",
		"certificates/acme/b.com/b.com.crt",
		"certificates-other/x",
		"acme/acct/users/me.json",
	} {
		if err := s.Store(ctx, k, []byte(k)); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.List(ctx, "certificates/acme", false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"certificates/acme/a.com", "certificates/acme/b.com"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("non-recursive: got %v want %v", got, want)
	}

	got, err = s.List(ctx, "certificates/acme/", true)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{
		"certificates/acme/a.com/a.com.crt",
		"certificates/acme/a.com/a.com.key",
		"certificates/acme/b.com/b.com.crt",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("recursive: got %v want %v", got, want)
	}

	// Sibling prefix "certificates-other" must not leak into "certificates".
	got, _ = s.List(ctx, "certificates", true)
	if len(got) != 3 {
		t.Fatalf("prefix isolation: got %v", got)
	}

	// Root listing with empty prefix returns everything.
	got, _ = s.List(ctx, "", true)
	if len(got) != 5 {
		t.Fatalf("root recursive: got %v", got)
	}
	got, _ = s.List(ctx, "", false)
	if !reflect.DeepEqual(got, []string{"acme", "certificates", "certificates-other"}) {
		t.Fatalf("root non-recursive: got %v", got)
	}

	if _, err := s.List(ctx, "missing", true); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("List missing: %v", err)
	}

	// Deleting a directory removes everything under it.
	if err := s.Delete(ctx, "certificates/acme/a.com"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.List(ctx, "certificates/acme", true)
	if !reflect.DeepEqual(got, []string{"certificates/acme/b.com/b.com.crt"}) {
		t.Fatalf("after dir delete: %v", got)
	}
}

func TestStorageProposeErrors(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	transport := NewStorage(db, func(context.Context, meta.Command) (meta.Result, error) {
		return meta.Result{}, errors.New("raft: not leader")
	}, nil)
	if err := transport.Store(ctx, "k", nil); err == nil || !errors.Is(err, err) {
		t.Fatalf("want transport error, got %v", err)
	}
	if err := transport.Delete(ctx, "k"); err == nil {
		t.Fatal("Delete: want transport error")
	}

	business := NewStorage(db, func(context.Context, meta.Command) (meta.Result, error) {
		return meta.Result{Err: meta.CodeInternal, Msg: "boom"}, nil
	}, nil)
	err := business.Store(ctx, "k", nil)
	var me *meta.Error
	if !errors.As(err, &me) || me.Code != meta.CodeInternal {
		t.Fatalf("want meta.Error, got %v", err)
	}
}

func TestStorageUsesRealClockByDefault(t *testing.T) {
	db := openDB(t)
	s := NewStorage(db, directPropose(db), nil)
	before := time.Now().Add(-time.Second)
	if err := s.Store(context.Background(), "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	info, err := s.Stat(context.Background(), "k")
	if err != nil || info.Modified.Before(before) {
		t.Fatalf("Stat: %+v %v", info, err)
	}
}
