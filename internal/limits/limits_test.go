package limits

import (
	"bytes"
	"errors"
	"io"
	"math"
	"os"
	"strings"
	"testing"
)

func TestDefaultsAreFiniteAndDocumented(t *testing.T) {
	defaults := Defaults()
	if err := defaults.Validate(); err != nil {
		t.Fatalf("Defaults().Validate() error = %v", err)
	}
	want := map[string]int64{
		StructuredBytesKey: DefaultStructuredBytes,
		ChecksumBytesKey:   DefaultChecksumBytes,
		DownloadBytesKey:   DefaultDownloadBytes,
		BinaryBytesKey:     DefaultBinaryBytes,
		ArchiveEntriesKey:  DefaultArchiveEntries,
		ArchiveBytesKey:    DefaultArchiveBytes,
	}
	for _, key := range Keys() {
		got, ok := defaults.Value(key)
		if !ok || got != want[key] {
			t.Errorf("Defaults().Value(%q) = (%d, %v), want %d", key, got, ok, want[key])
		}
	}
	keys := Keys()
	keys[0] = "changed"
	if Keys()[0] == "changed" {
		t.Fatal("Keys() returned an aliased slice")
	}
	if IsKey("unknown") || IsByteKey("unknown") || IsByteKey(ArchiveEntriesKey) {
		t.Fatal("key classification accepted an invalid or count key")
	}
	if _, ok := defaults.Value("unknown"); ok {
		t.Fatal("Value() found an unknown key")
	}
}

func TestParseForKey(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		value   string
		want    int64
		errText string
	}{
		{name: "plain bytes", key: DownloadBytesKey, value: "123", want: 123},
		{name: "bytes suffix", key: DownloadBytesKey, value: "2 MiB", want: 2 * MiB},
		{name: "case insensitive suffix", key: BinaryBytesKey, value: "3gib", want: 3 * GiB},
		{name: "count", key: ArchiveEntriesKey, value: "10000", want: 10000},
		{name: "count rejects unit", key: ArchiveEntriesKey, value: "10KiB", errText: "not valid for a count"},
		{name: "zero", key: BinaryBytesKey, value: "0", errText: "positive"},
		{name: "negative", key: BinaryBytesKey, value: "-1", errText: "positive integer"},
		{name: "decimal", key: BinaryBytesKey, value: "1.5MiB", errText: "unknown size unit"},
		{name: "unknown unit", key: BinaryBytesKey, value: "1KB", errText: "unknown size unit"},
		{name: "unlimited", key: BinaryBytesKey, value: "unlimited", errText: "unlimited"},
		{name: "overflow digits", key: BinaryBytesKey, value: "9223372036854775808", errText: "overflows int64"},
		{name: "overflow multiplier", key: BinaryBytesKey, value: "9223372036854775807KiB", errText: "overflows int64"},
		{name: "unknown key", key: "max_unknown_bytes", value: "1", errText: "unknown limit key"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseForKey(test.key, test.value)
			if test.errText != "" {
				if err == nil || !strings.Contains(err.Error(), test.errText) {
					t.Fatalf("ParseForKey() error = %v, want %q", err, test.errText)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseForKey() error = %v", err)
			}
			if got != test.want {
				t.Errorf("ParseForKey() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestLimitsSetAndValidate(t *testing.T) {
	values := Defaults()
	if err := values.Set(DownloadBytesKey, 123); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if got, _ := values.Value(DownloadBytesKey); got != 123 {
		t.Errorf("download limit = %d, want 123", got)
	}
	for _, test := range []struct {
		name  string
		key   string
		value int64
	}{
		{name: "zero", key: BinaryBytesKey, value: 0},
		{name: "negative", key: BinaryBytesKey, value: -1},
		{name: "unknown", key: "unknown", value: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := values.Set(test.key, test.value); err == nil {
				t.Fatal("Set() error = nil, want validation error")
			}
		})
	}
	values.BinaryBytes = 0
	if err := values.Validate(); err == nil {
		t.Fatal("Validate() error = nil for zero limit")
	}
	var nilValues *Limits
	if err := nilValues.Set(BinaryBytesKey, 1); err == nil {
		t.Fatal("nil Set() error = nil, want error")
	}
	for index, key := range Keys() {
		if err := values.Set(key, int64(index+1)); err != nil {
			t.Fatalf("Set(%q) error = %v", key, err)
		}
		if got, _ := values.Value(key); got != int64(index+1) {
			t.Errorf("Value(%q) = %d, want %d", key, got, index+1)
		}
	}
}

func TestReaderRejectsTheFirstByteOverLimit(t *testing.T) {
	data, err := ReadAll(strings.NewReader("12345"), 4, "binary candidate bytes", "asset-name")
	if err == nil {
		t.Fatal("ReadAll() error = nil, want limit error")
	}
	if !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("ReadAll() error = %v, want ErrLimitExceeded", err)
	}
	if !strings.Contains(err.Error(), "binary candidate bytes limit=4") || !strings.Contains(err.Error(), "asset-name") {
		t.Errorf("ReadAll() error = %q, want limit and input", err)
	}
	if data != nil {
		t.Fatalf("ReadAll() data = %q, want nil on error", data)
	}

	reader := NewReader(strings.NewReader("ok"), math.MaxInt64, "test", "input")
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatalf("MaxInt64 reader error = %v", err)
	}
	if reader.ReadBytes() != 2 {
		t.Errorf("ReadBytes() = %d, want 2", reader.ReadBytes())
	}
	if got, err := Parse(DownloadBytesKey, "1KiB"); err != nil || got != KiB {
		t.Errorf("Parse() = (%d, %v), want (%d, nil)", got, err, KiB)
	}
	var nilError *LimitError
	if nilError.Error() != ErrLimitExceeded.Error() {
		t.Fatalf("nil LimitError.Error() = %q", nilError.Error())
	}
	if _, err := ReadAll(strings.NewReader("data"), 0, "test", "input"); err == nil {
		t.Fatal("ReadAll() error = nil for zero limit")
	}
	if _, err := NewBudget(0, "archive", "input"); err == nil {
		t.Fatal("NewBudget() error = nil for zero limit")
	}
}

func TestBudgetCountsAcrossReadersAndCheckCount(t *testing.T) {
	budget, err := NewBudget(5, "archive decompressed bytes", "archive.tar")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, budget.Reader(strings.NewReader("abc"))); err != nil {
		t.Fatalf("first budget read: %v", err)
	}
	if _, err := io.Copy(io.Discard, budget.Reader(strings.NewReader("def"))); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("second budget read error = %v, want limit error", err)
	}
	if budget.Used() != 5 || budget.Remaining() != 0 {
		t.Fatalf("budget usage = (%d, %d), want (5, 0)", budget.Used(), budget.Remaining())
	}
	if err := CheckCount(2, 2, "archive entry count", "archive.tar"); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("CheckCount() error = %v, want limit error", err)
	}
	if err := CheckCount(1, 0, "archive entry count", "archive.tar"); err == nil {
		t.Fatal("CheckCount() error = nil for zero limit")
	}
}

func TestDecodeJSONBoundsAndRejectsDuplicateKeys(t *testing.T) {
	var value map[string]int
	if err := DecodeJSON(bytes.NewBufferString(`{"ok":1}`), &value, 32, "structured JSON", "manifest.json"); err != nil {
		t.Fatalf("DecodeJSON() valid input error = %v", err)
	}
	if value["ok"] != 1 {
		t.Fatalf("decoded value = %#v, want ok=1", value)
	}
	if err := DecodeJSON(bytes.NewBufferString(`{"ok":1,"ok":2}`), &value, 32, "structured JSON", "manifest.json"); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("DecodeJSON() duplicate error = %v, want duplicate-key error", err)
	}
	if err := DecodeJSON(strings.NewReader(strings.Repeat("x", 9)), &value, 8, "structured JSON", "manifest.json?token=secret"); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("DecodeJSON() oversized error = %v, want limit error", err)
	} else if strings.Contains(err.Error(), "secret") {
		t.Fatalf("DecodeJSON() leaked query in error: %v", err)
	}
}

func TestReadFileAndDecodeJSONFile(t *testing.T) {
	path := t.TempDir() + "/input.json"
	if err := os.WriteFile(path, []byte(`{"name":"tool"}`), 0644); err != nil {
		t.Fatal(err)
	}
	data, err := ReadFile(path, 64, "structured JSON")
	if err != nil || !bytes.Equal(data, []byte(`{"name":"tool"}`)) {
		t.Fatalf("ReadFile() = (%q, %v)", data, err)
	}
	var value struct {
		Name string `json:"name"`
	}
	if err := DecodeJSONFile(path, &value, 64, "structured JSON"); err != nil {
		t.Fatalf("DecodeJSONFile() error = %v", err)
	}
	if value.Name != "tool" {
		t.Fatalf("DecodeJSONFile() value = %#v", value)
	}
}
