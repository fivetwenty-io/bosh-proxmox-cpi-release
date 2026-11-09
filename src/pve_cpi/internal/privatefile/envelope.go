package privatefile

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
)

// Envelope is the outer wrapper of a stored value: a schema version, the
// integrity string of the payload, and the payload's JSON. On disk the
// integrity string sits under the Policy's IntegrityKey.
type Envelope struct {
	Version   int
	Integrity string
	Payload   json.RawMessage
}

// wireType is the struct the envelope is encoded and strictly decoded
// through. Building it from the Policy's key gives each store its own field
// name with the field order and strictness of a hand-written struct.
func (p Policy) wireType() reflect.Type {
	return reflect.StructOf([]reflect.StructField{
		{Name: "Version", Type: reflect.TypeFor[int](), Tag: `json:"version"`},
		{Name: "Integrity", Type: reflect.TypeFor[string](), Tag: reflect.StructTag(`json:"` + p.IntegrityKey + `"`)},
		{Name: "Payload", Type: reflect.TypeFor[json.RawMessage](), Tag: `json:"payload"`},
	})
}

func (p Policy) decodeWire(data []byte) (Envelope, error) {
	wire := reflect.New(p.wireType())
	if err := StrictDecode(data, wire.Interface()); err != nil {
		return Envelope{}, err
	}
	fields := wire.Elem()
	return Envelope{
		Version:   int(fields.Field(0).Int()),
		Integrity: fields.Field(1).String(),
		Payload:   json.RawMessage(fields.Field(2).Bytes()),
	}, nil
}

func (p Policy) encodeWire(env Envelope) ([]byte, error) {
	wire := reflect.New(p.wireType()).Elem()
	wire.Field(0).SetInt(int64(env.Version))
	wire.Field(1).SetString(env.Integrity)
	wire.Field(2).SetBytes(env.Payload)
	return json.Marshal(wire.Interface())
}

// StrictDecode decodes one JSON value into out, refusing unknown fields and
// any data after the value.
func StrictDecode(data []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("trailing JSON data")
	}
	return nil
}

// ReadRetries bounds how many times ReadEnvelope reopens a file whose inode
// was replaced between the lstat and the fstat inside Open. A concurrent
// atomic rename is exactly what WriteEnvelope is built to do, so a reader
// losing that race is ordinary and deserves another look rather than a
// reconciliation record. Writers hold the exclusive lock and are not exposed
// this way. The bound keeps a genuinely unstable file from spinning: after
// this many losses the Changed error is returned to the caller unchanged.
const ReadRetries = 3

// ReadFile opens name under the private-file rules and returns its bytes. A
// file longer than limit is refused as corrupt, and a file replaced during the
// open is reopened up to ReadRetries times before the Changed error comes back.
func (p Policy) ReadFile(r *os.Root, name string, limit int) ([]byte, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("%s: size limit not set", p.label())
	}
	f, err := p.Open(r, name, os.O_RDONLY)
	for attempt := 0; attempt < ReadRetries && errors.Is(err, p.changed()); attempt++ {
		f, err = p.Open(r, name, os.O_RDONLY)
	}
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	closeErr := f.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("%w: oversized file", p.corrupt())
	}
	return data, nil
}

// ReadEnvelope reads name within the size limit and decodes the outer
// envelope without judging its version, so a caller whose file versions on
// its own schedule can tell a newer file from a damaged one before it decides
// what to do.
func (p Policy) ReadEnvelope(r *os.Root, name string) (Envelope, error) {
	if err := p.check(); err != nil {
		return Envelope{}, err
	}
	data, err := p.ReadFile(r, name, p.MaxBytes)
	if err != nil {
		return Envelope{}, err
	}
	env, err := p.decodeWire(data)
	if err != nil {
		return Envelope{}, fmt.Errorf("%w: malformed envelope: %w", p.corrupt(), err)
	}
	return env, nil
}

// DecodeEnvelope checks env against the expected version and its integrity
// string and strictly decodes the payload into out.
func (p Policy) DecodeEnvelope(env Envelope, version int, out any) error {
	if err := p.check(); err != nil {
		return err
	}
	if env.Version != version || subtle.ConstantTimeCompare([]byte(env.Integrity), []byte(p.Integrity(env.Payload))) != 1 {
		return fmt.Errorf("%w: envelope version/checksum", p.corrupt())
	}
	if err := StrictDecode(env.Payload, out); err != nil {
		return fmt.Errorf("%w: malformed payload: %w", p.corrupt(), err)
	}
	return nil
}

// WriteEnvelope marshals value into an envelope of the given version and
// replaces name with it through WriteFile.
func (p Policy) WriteEnvelope(r *os.Root, name string, version int, value any, ops Ops) error {
	if err := p.check(); err != nil {
		return err
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data, err := p.encodeWire(Envelope{Version: version, Integrity: p.Integrity(payload), Payload: payload})
	if err != nil {
		return err
	}
	return p.WriteFile(r, name, data, ops)
}

// TempPrefix starts the name of the temporary file WriteFile creates, so a
// sweep can recognize the leftovers of a write that was cut short.
const TempPrefix = ".tmp-"

// WriteFile replaces name with data atomically: it writes a temporary file
// opened with O_EXCL, syncs it, renames it over name, and syncs the
// directory, each through ops. Data longer than MaxBytes is refused. A failure
// may follow a committed rename, so a caller that needs durability treats any
// error as an unknown outcome.
func (p Policy) WriteFile(r *os.Root, name string, data []byte, ops Ops) (retErr error) {
	if err := p.checkSize(); err != nil {
		return err
	}
	ops = ops.withDefaults()
	if len(data) > p.MaxBytes {
		return fmt.Errorf("%s: record exceeds size limit", p.label())
	}
	if info, err := r.Lstat(name); err == nil {
		if err := p.PrivateInfo(filepath.Join(r.Name(), name), info, false); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	id, err := p.newID()
	if err != nil {
		return err
	}
	temp := TempPrefix + id
	f, err := p.Open(r, temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		return err
	}
	closed := false
	renamed := false
	defer func() {
		if !closed {
			retErr = errors.Join(retErr, f.Close())
		}
		if !renamed {
			retErr = errors.Join(retErr, r.Remove(temp))
		}
	}()
	n, err := ops.Write(f, data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	if err := ops.SyncFile(f); err != nil {
		return err
	}
	err = f.Close()
	closed = true
	if err != nil {
		return err
	}
	if err := ops.Rename(r, temp, name); err != nil {
		return err
	}
	renamed = true
	return ops.SyncDir(r)
}
