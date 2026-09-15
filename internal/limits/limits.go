// Package limits defines the finite resource budgets used while reading
// external input and release assets.
package limits

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
)

const (
	// KiB, MiB, and GiB are binary, IEC-sized units used by the configuration
	// parser and the documented default values.
	KiB int64 = 1024
	MiB       = 1024 * KiB
	GiB       = 1024 * MiB

	// DefaultStructuredBytes bounds one structured JSON input.
	DefaultStructuredBytes int64 = 4 * MiB
	// DefaultChecksumBytes bounds one checksum asset.
	DefaultChecksumBytes int64 = 8 * MiB
	// DefaultDownloadBytes bounds one release asset download.
	DefaultDownloadBytes int64 = 1 * GiB
	// DefaultBinaryBytes bounds one extracted binary candidate.
	DefaultBinaryBytes int64 = 512 * MiB
	// DefaultArchiveEntries bounds the number of entries inspected in one archive.
	DefaultArchiveEntries int64 = 10_000
	// DefaultArchiveBytes bounds the decompressed data read from one archive.
	DefaultArchiveBytes int64 = 2 * GiB

	// Canonical configuration keys.
	StructuredBytesKey = "max_structured_bytes"
	ChecksumBytesKey   = "max_checksum_bytes"
	DownloadBytesKey   = "max_download_bytes"
	BinaryBytesKey     = "max_binary_bytes"
	ArchiveEntriesKey  = "max_archive_entries"
	ArchiveBytesKey    = "max_archive_bytes"
)

// Limits contains all effective finite resource limits used by zenget.
type Limits struct {
	StructuredBytes int64
	ChecksumBytes   int64
	DownloadBytes   int64
	BinaryBytes     int64
	ArchiveEntries  int64
	ArchiveBytes    int64
}

// Defaults returns the documented default limits.
func Defaults() Limits {
	return Limits{
		StructuredBytes: DefaultStructuredBytes,
		ChecksumBytes:   DefaultChecksumBytes,
		DownloadBytes:   DefaultDownloadBytes,
		BinaryBytes:     DefaultBinaryBytes,
		ArchiveEntries:  DefaultArchiveEntries,
		ArchiveBytes:    DefaultArchiveBytes,
	}
}

var limitKeys = []string{
	StructuredBytesKey,
	ChecksumBytesKey,
	DownloadBytesKey,
	BinaryBytesKey,
	ArchiveEntriesKey,
	ArchiveBytesKey,
}

// Keys returns the canonical configuration keys in stable display order.
func Keys() []string {
	keys := make([]string, len(limitKeys))
	copy(keys, limitKeys)
	return keys
}

// IsKey reports whether key names a supported resource limit.
func IsKey(key string) bool {
	for _, candidate := range limitKeys {
		if key == candidate {
			return true
		}
	}
	return false
}

// IsByteKey reports whether key accepts an IEC byte suffix.
func IsByteKey(key string) bool {
	return key != ArchiveEntriesKey && IsKey(key)
}

// Validate checks that every effective limit is finite and positive.
func (l Limits) Validate() error {
	values := []struct {
		key   string
		value int64
	}{
		{StructuredBytesKey, l.StructuredBytes},
		{ChecksumBytesKey, l.ChecksumBytes},
		{DownloadBytesKey, l.DownloadBytes},
		{BinaryBytesKey, l.BinaryBytes},
		{ArchiveEntriesKey, l.ArchiveEntries},
		{ArchiveBytesKey, l.ArchiveBytes},
	}
	for _, item := range values {
		if item.value <= 0 {
			return fmt.Errorf("%s must be a positive int64", item.key)
		}
	}
	return nil
}

// Value returns the configured value for key.
func (l Limits) Value(key string) (int64, bool) {
	switch key {
	case StructuredBytesKey:
		return l.StructuredBytes, true
	case ChecksumBytesKey:
		return l.ChecksumBytes, true
	case DownloadBytesKey:
		return l.DownloadBytes, true
	case BinaryBytesKey:
		return l.BinaryBytes, true
	case ArchiveEntriesKey:
		return l.ArchiveEntries, true
	case ArchiveBytesKey:
		return l.ArchiveBytes, true
	default:
		return 0, false
	}
}

// Set updates one limit and rejects unknown, zero, negative, or otherwise
// invalid values.
func (l *Limits) Set(key string, value int64) error {
	if l == nil {
		return errors.New("limits are nil")
	}
	if !IsKey(key) {
		return fmt.Errorf("unknown limit key %q", key)
	}
	if value <= 0 {
		return fmt.Errorf("%s must be a positive int64", key)
	}
	switch key {
	case StructuredBytesKey:
		l.StructuredBytes = value
	case ChecksumBytesKey:
		l.ChecksumBytes = value
	case DownloadBytesKey:
		l.DownloadBytes = value
	case BinaryBytesKey:
		l.BinaryBytes = value
	case ArchiveEntriesKey:
		l.ArchiveEntries = value
	case ArchiveBytesKey:
		l.ArchiveBytes = value
	}
	return nil
}

// ParseForKey parses a positive decimal integer or, for byte limits, a
// positive integer with an optional B, KiB, MiB, or GiB suffix. The result is
// always an int64 byte/count value suitable for deterministic JSON storage.
func ParseForKey(key, value string) (int64, error) {
	if !IsKey(key) {
		return 0, fmt.Errorf("unknown limit key %q", key)
	}
	return parseValue(value, IsByteKey(key))
}

// Parse is an alias for ParseForKey for callers that prefer a shorter name.
func Parse(key, value string) (int64, error) {
	return ParseForKey(key, value)
}

func parseValue(value string, allowIEC bool) (int64, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return 0, errors.New("value must be a positive integer")
	}
	if strings.EqualFold(raw, "unlimited") || strings.EqualFold(raw, "none") {
		return 0, errors.New("unlimited values are not supported")
	}

	digitEnd := 0
	for digitEnd < len(raw) && raw[digitEnd] >= '0' && raw[digitEnd] <= '9' {
		digitEnd++
	}
	if digitEnd == 0 {
		return 0, fmt.Errorf("%q is not a positive integer", value)
	}
	digits := raw[:digitEnd]
	suffix := strings.TrimSpace(raw[digitEnd:])
	if suffix != "" && !allowIEC {
		return 0, fmt.Errorf("unit %q is not valid for a count limit", suffix)
	}

	multiplier := int64(1)
	switch {
	case suffix == "" && allowIEC:
	case suffix == "" && !allowIEC:
	case strings.EqualFold(suffix, "B") && allowIEC:
	case strings.EqualFold(suffix, "KiB") && allowIEC:
		multiplier = KiB
	case strings.EqualFold(suffix, "MiB") && allowIEC:
		multiplier = MiB
	case strings.EqualFold(suffix, "GiB") && allowIEC:
		multiplier = GiB
	case suffix != "":
		return 0, fmt.Errorf("unknown size unit %q; use B, KiB, MiB, or GiB", suffix)
	}

	base, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || base <= 0 {
		if err != nil && errors.Is(err, strconv.ErrRange) {
			return 0, fmt.Errorf("value %q overflows int64", value)
		}
		return 0, fmt.Errorf("%q is not a positive integer", value)
	}
	if base > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("value %q overflows int64", value)
	}
	return base * multiplier, nil
}

// ErrLimitExceeded identifies a bounded read that encountered more input than
// its configured finite limit.
var ErrLimitExceeded = errors.New("resource limit exceeded")

// LimitError describes a resource limit violation without including sensitive
// URL query data in the input identifier.
type LimitError struct {
	Kind  string
	Limit int64
	Input string
}

func (e *LimitError) Error() string {
	if e == nil {
		return ErrLimitExceeded.Error()
	}
	input := safeInput(e.Input)
	if input == "" {
		input = "<input>"
	}
	return fmt.Sprintf("resource limit exceeded: %s limit=%d input=%q", e.Kind, e.Limit, input)
}

// Unwrap allows errors.Is(err, ErrLimitExceeded) checks.
func (e *LimitError) Unwrap() error {
	return ErrLimitExceeded
}

func newLimitError(kind string, limit int64, input string) *LimitError {
	return &LimitError{Kind: kind, Limit: limit, Input: safeInput(input)}
}

func safeInput(input string) string {
	if idx := strings.IndexAny(input, "?#"); idx >= 0 {
		return input[:idx]
	}
	return input
}

// Reader counts bytes read from source and probes one byte after the limit so
// a missing or false Content-Length cannot bypass the budget. It never uses a
// limit+1 arithmetic operation, so MaxInt64 remains safe.
type Reader struct {
	source io.Reader
	limit  int64
	read   int64
	kind   string
	input  string
}

// NewReader returns a streaming reader with a finite byte limit.
func NewReader(source io.Reader, limit int64, kind, input string) *Reader {
	if source == nil {
		source = strings.NewReader("")
	}
	return &Reader{source: source, limit: limit, kind: kind, input: input}
}

// Read implements io.Reader and returns a LimitError as soon as one byte over
// the configured limit is observed.
func (r *Reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.limit <= 0 {
		return 0, newLimitError(r.kind, r.limit, r.input)
	}
	if r.read >= r.limit {
		var probe [1]byte
		n, err := r.source.Read(probe[:])
		if n > 0 {
			return 0, newLimitError(r.kind, r.limit, r.input)
		}
		return 0, err
	}

	remaining := r.limit - r.read
	readSize := int64(len(p))
	if readSize > remaining {
		readSize = remaining
	}
	n, err := r.source.Read(p[:int(readSize)])
	if n > 0 {
		r.read += int64(n)
	}
	return n, err
}

// ReadBytes returns the number of bytes accepted before the last read.
func (r *Reader) ReadBytes() int64 {
	if r == nil {
		return 0
	}
	return r.read
}

// ReadAll reads at most max bytes and fails if source contains one additional
// byte. The returned data is nil on any error.
func ReadAll(source io.Reader, max int64, kind, input string) ([]byte, error) {
	if err := validateLimit(max, kind); err != nil {
		return nil, err
	}
	reader := NewReader(source, max, kind, input)
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// ReadFile reads a local file through the same bounded reader used for
// external input.
func ReadFile(path string, max int64, kind string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return ReadAll(file, max, kind, path)
}

func validateLimit(limit int64, kind string) error {
	if limit <= 0 {
		return fmt.Errorf("%s limit must be a positive int64", kind)
	}
	return nil
}

// Budget shares one cumulative decompressed-byte limit across multiple entry
// readers in an archive.
type Budget struct {
	limit int64
	used  int64
	kind  string
	input string
}

// NewBudget creates a cumulative byte budget.
func NewBudget(limit int64, kind, input string) (*Budget, error) {
	if err := validateLimit(limit, kind); err != nil {
		return nil, err
	}
	return &Budget{limit: limit, kind: kind, input: input}, nil
}

// Reader wraps source in the cumulative budget.
func (b *Budget) Reader(source io.Reader) io.Reader {
	if b == nil {
		return source
	}
	if source == nil {
		source = strings.NewReader("")
	}
	return &budgetReader{source: source, budget: b}
}

// Used returns the number of decompressed bytes consumed by the budget.
func (b *Budget) Used() int64 {
	if b == nil {
		return 0
	}
	return b.used
}

// Remaining returns the number of bytes available before the budget is full.
func (b *Budget) Remaining() int64 {
	if b == nil {
		return 0
	}
	return b.limit - b.used
}

type budgetReader struct {
	source io.Reader
	budget *Budget
}

func (r *budgetReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.budget.used >= r.budget.limit {
		var probe [1]byte
		n, err := r.source.Read(probe[:])
		if n > 0 {
			return 0, newLimitError(r.budget.kind, r.budget.limit, r.budget.input)
		}
		return 0, err
	}

	remaining := r.budget.limit - r.budget.used
	readSize := int64(len(p))
	if readSize > remaining {
		readSize = remaining
	}
	n, err := r.source.Read(p[:int(readSize)])
	if n > 0 {
		r.budget.used += int64(n)
	}
	return n, err
}

// CheckCount rejects the next item when count already reached limit. It is
// intentionally based on comparison rather than count+1 to avoid overflow.
func CheckCount(count, limit int64, kind, input string) error {
	if limit <= 0 {
		return fmt.Errorf("%s limit must be a positive int64", kind)
	}
	if count < 0 {
		return fmt.Errorf("%s count must not be negative", kind)
	}
	if count >= limit {
		return newLimitError(kind, limit, input)
	}
	return nil
}

// DecodeJSON bounds source before decoding and rejects duplicate object keys.
// Callers retain control over schema and unknown-field policy by decoding into
// their own destination type after this common safety check.
func DecodeJSON(source io.Reader, destination any, max int64, kind, input string) error {
	data, err := ReadAll(source, max, kind, input)
	if err != nil {
		return err
	}
	if err := rejectDuplicateKeys(data); err != nil {
		return fmt.Errorf("decode %s %q: %w", kind, safeInput(input), err)
	}
	if err := json.Unmarshal(data, destination); err != nil {
		return fmt.Errorf("decode %s %q: %w", kind, safeInput(input), err)
	}
	return nil
}

// DecodeJSONFile is the local-file counterpart of DecodeJSON.
func DecodeJSONFile(path string, destination any, max int64, kind string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return DecodeJSON(file, destination, max, kind, path)
}

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := validateJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func validateJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}

	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate JSON object key %q", key)
			}
			seen[key] = struct{}{}
			if err := validateJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return errors.New("malformed JSON object")
		}
	case '[':
		for decoder.More() {
			if err := validateJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return errors.New("malformed JSON array")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
	return nil
}
