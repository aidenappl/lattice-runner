package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"
)

// Compression was added to the snapshot path without the restore path being
// taught about it, so restores fed gzip bytes into the SQL client and failed
// with `ASCII '\0' appeared in the statement`. A backup that cannot be restored
// is not a backup, and that failure is invisible until the day it matters.
func TestMaybeGunzipDecompressesGzippedSnapshots(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte("CREATE TABLE proof (id INT);")); err != nil {
		t.Fatal(err)
	}
	gz.Close()

	r, err := maybeGunzip(&buf)
	if err != nil {
		t.Fatalf("maybeGunzip: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(out), "CREATE TABLE proof") {
		t.Errorf("did not decompress: %q", string(out))
	}
}

// Snapshots taken before compression existed are plain SQL and must stay
// restorable — this is why detection is by content and not by filename.
func TestMaybeGunzipPassesPlainSQLThrough(t *testing.T) {
	const sql = "INSERT INTO proof VALUES (1);"

	r, err := maybeGunzip(strings.NewReader(sql))
	if err != nil {
		t.Fatalf("maybeGunzip: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(out) != sql {
		t.Errorf("plain SQL was altered: %q", string(out))
	}
}

// The .sql.gz extension was a lie for months — it claimed gzip while the bytes
// were plain SQL. Trusting the name is exactly the wrong instinct, so the
// detector must ignore it entirely.
func TestMaybeGunzipIgnoresFilenameAndTrustsBytes(t *testing.T) {
	// Content is plain SQL despite any .gz naming upstream.
	r, err := maybeGunzip(strings.NewReader("-- dump\nSELECT 1;"))
	if err != nil {
		t.Fatalf("maybeGunzip: %v", err)
	}
	out, _ := io.ReadAll(r)
	if !strings.HasPrefix(string(out), "-- dump") {
		t.Errorf("content was mangled: %q", string(out))
	}
}

func TestMaybeGunzipHandlesEmptyInput(t *testing.T) {
	r, err := maybeGunzip(strings.NewReader(""))
	if err != nil {
		t.Fatalf("an empty artifact should not error here: %v", err)
	}
	out, _ := io.ReadAll(r)
	if len(out) != 0 {
		t.Errorf("expected no output, got %q", string(out))
	}
}
