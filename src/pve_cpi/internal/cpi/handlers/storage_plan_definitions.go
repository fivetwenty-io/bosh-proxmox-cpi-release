package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// StorageDefinitions holds the storage definitions a plan freezes, keyed by
// storage ID. It owns its JSON form, because the plan is durable journal
// evidence that every release must read, while pve.StorageInfo's JSON tags
// belong to the HA profile cache file.
//
// Releases up to 0.9.x wrote each definition with the Go field names of an
// untagged pve.StorageInfo ("Name", "IsMountpoint"). Releases 0.10.0 through
// 0.10.2 wrote the snake_case keys of the HA profile cache ("name",
// "is_mountpoint"), which a 0.9.x strict decoder refuses. This type writes
// the 0.9.x form again, so a director rolled back to 0.9.x still reads what
// this release wrote, and it reads both forms, entry by entry.
//
// Decoding is strict, because a custom UnmarshalJSON does not inherit the
// outer decoder's DisallowUnknownFields. A key that neither form defines, two
// keys that name the same field, and a null mount flag all fail the decode.
// The sibling plan decode reads through lenientStorageDefinitions instead.
type StorageDefinitions map[string]pve.StorageInfo

// storageDefinitionWire is pve.StorageInfo exactly as releases up to 0.9.x
// encoded it inside a plan. It carries no tags, so each key is the Go field
// name, and its fields keep pve.StorageInfo's order, so the bytes match too.
// The frozen-inputs fingerprint hashes this encoding.
type storageDefinitionWire struct {
	Name         string
	Type         string
	Shared       bool
	Disabled     bool
	Nodes        []string
	Path         string
	Server       string
	Export       string
	Content      string
	IsMountpoint bool
}

// storageDefinitionSnakeWire is pve.StorageInfo as releases 0.10.0 through
// 0.10.2 encoded it inside a plan. It is history, and it exists only to
// recompute the frozen-inputs fingerprints those releases stored. If
// pve.StorageInfo gains a field, do not add it here. Convert to this type
// field by field instead, because a fingerprint those releases wrote covers
// these ten fields and no others.
type storageDefinitionSnakeWire struct {
	Name         string   `json:"name"`
	Type         string   `json:"type"`
	Shared       bool     `json:"shared"`
	Disabled     bool     `json:"disabled"`
	Nodes        []string `json:"nodes"`
	Path         string   `json:"path"`
	Server       string   `json:"server"`
	Export       string   `json:"export"`
	Content      string   `json:"content"`
	IsMountpoint bool     `json:"is_mountpoint"`
}

// These conversions pin both mirrors to pve.StorageInfo. A struct conversion
// ignores tags but needs the same field names, types, and order, so a change
// to pve.StorageInfo's fields stops this package compiling until the plan's
// wire form is decided for it.
var (
	_ = storageDefinitionWire(pve.StorageInfo{})
	_ = storageDefinitionSnakeWire(pve.StorageInfo{})
)

// storageDefinitionDecode reads one definition in either form. The embedded
// mirror supplies the fields whose keys both forms share once Go's
// case-insensitive key matching applies ("Name" and "name", "Nodes" and
// "nodes"). The flag is the one key that differs by more than case, so it
// gets a field per form. The outer IsMountpoint shadows the embedded one, and
// pointers tell an absent key from false.
type storageDefinitionDecode struct {
	storageDefinitionWire
	IsMountpoint      *bool
	SnakeIsMountpoint *bool `json:"is_mountpoint"`
}

// MarshalJSON writes the 0.9.x form. A nil map stays null, as it was when the
// plan field was a plain map.
func (d StorageDefinitions) MarshalJSON() ([]byte, error) {
	if d == nil {
		return []byte("null"), nil
	}
	return json.Marshal(storageDefinitionsWire(d))
}

// UnmarshalJSON reads each definition in either form and refuses any key that
// neither form defines.
func (d *StorageDefinitions) UnmarshalJSON(data []byte) error {
	decoded, err := decodeStorageDefinitions(data, true)
	if err != nil {
		return err
	}
	*d = decoded
	return nil
}

// lenientStorageDefinitions is StorageDefinitions for the sibling plan
// decode. It reads both forms the same way, but it ignores a key it does not
// know, as the rest of that decode does, so a definition field a later
// release adds cannot break the sibling accounting of an older binary.
type lenientStorageDefinitions map[string]pve.StorageInfo

// UnmarshalJSON reads each definition in either form and ignores unknown keys.
func (d *lenientStorageDefinitions) UnmarshalJSON(data []byte) error {
	decoded, err := decodeStorageDefinitions(data, false)
	if err != nil {
		return err
	}
	*d = lenientStorageDefinitions(decoded)
	return nil
}

func storageDefinitionsWire(d map[string]pve.StorageInfo) map[string]storageDefinitionWire {
	if d == nil {
		return nil
	}
	out := make(map[string]storageDefinitionWire, len(d))
	for id := range d {
		out[id] = storageDefinitionWire(d[id])
	}
	return out
}

func storageDefinitionsSnakeWire(d map[string]pve.StorageInfo) map[string]storageDefinitionSnakeWire {
	if d == nil {
		return nil
	}
	out := make(map[string]storageDefinitionSnakeWire, len(d))
	for id := range d {
		out[id] = storageDefinitionSnakeWire(d[id])
	}
	return out
}

func decodeStorageDefinitions(data []byte, strict bool) (StorageDefinitions, error) {
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("storage definitions: %w", err)
	}
	if entries == nil {
		return nil, nil
	}
	out := make(StorageDefinitions, len(entries))
	for id, raw := range entries {
		info, err := decodeStorageDefinition(raw, strict)
		if err != nil {
			return nil, fmt.Errorf("storage definition %q: %w", id, err)
		}
		out[id] = info
	}
	return out, nil
}

func decodeStorageDefinition(raw json.RawMessage, strict bool) (pve.StorageInfo, error) {
	if strict {
		if err := checkStorageDefinitionKeys(raw); err != nil {
			return pve.StorageInfo{}, err
		}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	if strict {
		d.DisallowUnknownFields()
	}
	var entry storageDefinitionDecode
	if err := d.Decode(&entry); err != nil {
		return pve.StorageInfo{}, err
	}
	info := pve.StorageInfo(entry.storageDefinitionWire)
	// The strict path has already refused an entry that names the flag under
	// both keys. The lenient path reads the 0.9.x key first, which is the form
	// this release writes.
	switch {
	case entry.IsMountpoint != nil:
		info.IsMountpoint = *entry.IsMountpoint
	case entry.SnakeIsMountpoint != nil:
		info.IsMountpoint = *entry.SnakeIsMountpoint
	}
	return info, nil
}

// checkStorageDefinitionKeys refuses the entry shapes that no release writes
// and that a plain decode would settle silently. Two keys that name the same
// field once case is folded would leave the last one winning, the mount flag
// under both of its keys says the flag twice and may disagree with itself,
// and a null mount flag would read as false. Only the strict owner decode
// runs it. The sibling decode never reads the flag, so it keeps tolerating
// these shapes rather than refuse a create over another allocation's plan.
func checkStorageDefinitionKeys(raw json.RawMessage) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	open, err := d.Token()
	if err != nil {
		return err
	}
	if delim, ok := open.(json.Delim); !ok || delim != '{' {
		// A null entry or a non-object is left to the struct decode, which
		// reads null as an empty definition and refuses anything else.
		return nil
	}
	var keys []string
	var goFlag, snakeFlag bool
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return err
		}
		key, _ := token.(string)
		for _, seen := range keys {
			if strings.EqualFold(seen, key) {
				return fmt.Errorf("carries %q and %q, which name the same field", seen, key)
			}
		}
		keys = append(keys, key)
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return err
		}
		goKey := strings.EqualFold(key, "IsMountpoint")
		snakeKey := strings.EqualFold(key, "is_mountpoint")
		if (goKey || snakeKey) && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("carries a null %s", key)
		}
		goFlag = goFlag || goKey
		snakeFlag = snakeFlag || snakeKey
	}
	if goFlag && snakeFlag {
		return fmt.Errorf("carries both IsMountpoint and is_mountpoint")
	}
	return nil
}

// storageFrozenInputs is what a journal intent's frozen-inputs fingerprint
// hashes. The field names and order are the wire form every release has
// hashed, so they must not change.
type storageFrozenInputs struct {
	Nodes             []string
	Memberships       map[string][]string
	Definitions       StorageDefinitions
	PolicyFingerprint string
}

// fingerprint hashes the inputs with the definitions in the 0.9.x form, which
// is the form every release outside 0.10.0 through 0.10.2 writes.
func (f storageFrozenInputs) fingerprint() (string, error) {
	return allocationjournal.Fingerprint(f)
}

// snakeFingerprint hashes the inputs with the definitions in the form
// releases 0.10.0 through 0.10.2 hashed, so a record those releases opened
// can still be matched.
func (f storageFrozenInputs) snakeFingerprint() (string, error) {
	return allocationjournal.Fingerprint(struct {
		Nodes             []string
		Memberships       map[string][]string
		Definitions       map[string]storageDefinitionSnakeWire
		PolicyFingerprint string
	}{f.Nodes, f.Memberships, storageDefinitionsSnakeWire(f.Definitions), f.PolicyFingerprint})
}

// matches reports whether stored is the fingerprint of these inputs in either
// form a release has written.
func (f storageFrozenInputs) matches(stored string) (bool, error) {
	current, err := f.fingerprint()
	if err != nil {
		return false, err
	}
	if stored == current {
		return true, nil
	}
	snake, err := f.snakeFingerprint()
	if err != nil {
		return false, err
	}
	return stored == snake, nil
}
