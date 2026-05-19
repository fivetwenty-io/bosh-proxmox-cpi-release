package jsonrpc

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

// -----------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------

// readGolden returns the trimmed content of a testdata file.
func readGolden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("readGolden %q: %v", name, err)
	}
	return strings.TrimRight(string(b), "\n")
}

// captureEncode runs fn with a bytes.Buffer and returns trimmed output.
func captureEncode(t *testing.T, fn func(w io.Writer) error) string {
	t.Helper()
	var buf bytes.Buffer
	if err := fn(&buf); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return strings.TrimRight(buf.String(), "\n")
}

// -----------------------------------------------------------------------
// Decode tests
// -----------------------------------------------------------------------

func TestDecode_Valid(t *testing.T) {
	f, err := os.Open("testdata/request.json")
	if err != nil {
		t.Fatalf("open testdata/request.json: %v", err)
	}
	defer f.Close()

	req, err := Decode(f)
	if err != nil {
		t.Fatalf("Decode: unexpected error: %v", err)
	}

	if req.Method != "create_vm" {
		t.Errorf("Method = %q, want %q", req.Method, "create_vm")
	}
	if len(req.Arguments) != 6 {
		t.Errorf("len(Arguments) = %d, want 6", len(req.Arguments))
	}
	if req.Context.DirectorUUID != "550e8400-e29b-41d4-a716-446655440000" {
		t.Errorf("Context.DirectorUUID = %q, want 550e8400-...", req.Context.DirectorUUID)
	}
	if req.Context.RequestID != "cpi-abc123" {
		t.Errorf("Context.RequestID = %q, want cpi-abc123", req.Context.RequestID)
	}
	// Verify stemcell api_version encoded inside Context.VM map.
	vm, ok := req.Context.VM["stemcell"]
	if !ok {
		t.Fatal("Context.VM[\"stemcell\"] missing")
	}
	stemcellMap, ok := vm.(map[string]any)
	if !ok {
		t.Fatalf("Context.VM[\"stemcell\"] type = %T, want map[string]any", vm)
	}
	apiVer, ok := stemcellMap["api_version"]
	if !ok {
		t.Fatal("stemcell[\"api_version\"] missing")
	}
	// JSON numbers unmarshal as float64 into map[string]any.
	apiVerF, ok := apiVer.(float64)
	if !ok {
		t.Fatalf("api_version type = %T, want float64", apiVer)
	}
	if int(apiVerF) != 2 {
		t.Errorf("api_version = %v, want 2", apiVerF)
	}
	if req.APIVersion != 2 {
		t.Errorf("APIVersion = %d, want 2", req.APIVersion)
	}
}

func TestDecode_MissingMethod(t *testing.T) {
	input := `{"arguments":[],"context":{"director_uuid":"x","request_id":"y"}}`
	_, err := Decode(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for missing method, got nil")
	}
	if !strings.Contains(err.Error(), "method") {
		t.Errorf("error %q should mention \"method\"", err.Error())
	}
}

func TestDecode_MalformedJSON(t *testing.T) {
	_, err := Decode(strings.NewReader(`{not valid json`))
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
}

func TestDecode_EmptyInput(t *testing.T) {
	_, err := Decode(strings.NewReader(""))
	if err == nil {
		t.Fatal("expected error for empty input, got nil")
	}
}

func TestDecode_NilReader(t *testing.T) {
	_, err := Decode(nil)
	if err == nil {
		t.Fatal("expected error for nil reader, got nil")
	}
}

func TestDecode_EmptyMethod(t *testing.T) {
	input := `{"method":"","arguments":[],"context":{}}`
	_, err := Decode(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for empty method string, got nil")
	}
}

// -----------------------------------------------------------------------
// EncodeSuccess tests
// -----------------------------------------------------------------------

func TestEncodeSuccess(t *testing.T) {
	got := captureEncode(t, func(w io.Writer) error {
		return EncodeSuccess(w, "vm-cid-001", "")
	})

	// Unmarshal into Response struct and verify fields rather than doing a
	// string comparison (map literal marshals keys alphabetically, struct does not).
	var resp Response
	if err := json.Unmarshal([]byte(got), &resp); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if resp.Result != "vm-cid-001" {
		t.Errorf("result = %v, want vm-cid-001", resp.Result)
	}
	if resp.Error != nil {
		t.Errorf("error = %v, want nil", resp.Error)
	}
	if resp.Log != "" {
		t.Errorf("log = %q, want empty", resp.Log)
	}
}

func TestEncodeSuccess_MatchesGoldenFile(t *testing.T) {
	got := captureEncode(t, func(w io.Writer) error {
		return EncodeSuccess(w, "vm-cid-001", "")
	})

	// Golden file is compact JSON; strip trailing newline before compare.
	golden := readGolden(t, "response_ok.json")
	if got != golden {
		t.Errorf("EncodeSuccess golden mismatch\n got: %s\nwant: %s", got, golden)
	}
}

func TestEncodeSuccess_NilResult(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodeSuccess(&buf, nil, ""); err != nil {
		t.Fatalf("EncodeSuccess: %v", err)
	}
	raw := strings.TrimRight(buf.String(), "\n")
	// result must be JSON null.
	var resp map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if string(resp["result"]) != "null" {
		t.Errorf("result = %s, want null", resp["result"])
	}
	if string(resp["error"]) != "null" {
		t.Errorf("error = %s, want null", resp["error"])
	}
}

func TestEncodeSuccess_NilWriter(t *testing.T) {
	err := EncodeSuccess(nil, "x", "")
	if err == nil {
		t.Fatal("expected error for nil writer, got nil")
	}
}

func TestEncodeSuccess_WithLog(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodeSuccess(&buf, 42, "some log output"); err != nil {
		t.Fatalf("EncodeSuccess: %v", err)
	}
	var resp Response
	if err := json.Unmarshal(bytes.TrimRight(buf.Bytes(), "\n"), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Log != "some log output" {
		t.Errorf("Log = %q, want %q", resp.Log, "some log output")
	}
	if resp.Error != nil {
		t.Errorf("Error = %v, want nil", resp.Error)
	}
}

// -----------------------------------------------------------------------
// EncodeError tests
// -----------------------------------------------------------------------

func TestEncodeError(t *testing.T) {
	got := captureEncode(t, func(w io.Writer) error {
		return EncodeError(w, "Bosh::Clouds::CloudError", "something went wrong", false, "stack trace here")
	})

	golden := readGolden(t, "response_err.json")
	if got != golden {
		t.Errorf("EncodeError golden mismatch\n got: %s\nwant: %s", got, golden)
	}
}

func TestEncodeError_AllFields(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodeError(&buf,
		"Bosh::Clouds::RetriableCloudError",
		"disk not found: vol-abc",
		true,
		"debug info",
	); err != nil {
		t.Fatalf("EncodeError: %v", err)
	}

	var resp Response
	if err := json.Unmarshal(bytes.TrimRight(buf.Bytes(), "\n"), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Result != nil {
		t.Errorf("Result = %v, want nil", resp.Result)
	}
	if resp.Error == nil {
		t.Fatal("Error is nil")
	}
	if resp.Error.Type != "Bosh::Clouds::RetriableCloudError" {
		t.Errorf("Type = %q, want RetriableCloudError", resp.Error.Type)
	}
	if resp.Error.Message != "disk not found: vol-abc" {
		t.Errorf("Message = %q", resp.Error.Message)
	}
	if !resp.Error.OkToRetry {
		t.Error("OkToRetry = false, want true")
	}
	if resp.Log != "debug info" {
		t.Errorf("Log = %q, want %q", resp.Log, "debug info")
	}
}

func TestEncodeError_EmptyTypeDefaultsToCloudError(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodeError(&buf, "", "oops", false, ""); err != nil {
		t.Fatalf("EncodeError: %v", err)
	}
	var resp Response
	if err := json.Unmarshal(bytes.TrimRight(buf.Bytes(), "\n"), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("Error is nil")
	}
	if resp.Error.Type != "Bosh::Clouds::CloudError" {
		t.Errorf("Type = %q, want Bosh::Clouds::CloudError", resp.Error.Type)
	}
}

func TestEncodeError_NilWriter(t *testing.T) {
	err := EncodeError(nil, "Bosh::Clouds::CloudError", "msg", false, "")
	if err == nil {
		t.Fatal("expected error for nil writer, got nil")
	}
}

// -----------------------------------------------------------------------
// Round-trip test
// -----------------------------------------------------------------------

// TestRoundTrip decodes a request, encodes a success response, then decodes
// the response back to verify structural consistency across encode/decode.
func TestRoundTrip(t *testing.T) {
	// 1. Decode request from testdata.
	f, err := os.Open("testdata/request.json")
	if err != nil {
		t.Fatalf("open request.json: %v", err)
	}
	defer f.Close()

	req, err := Decode(f)
	if err != nil {
		t.Fatalf("Decode request: %v", err)
	}
	if req.Method != "create_vm" {
		t.Errorf("Method = %q, want create_vm", req.Method)
	}

	// 2. Encode a success response using a value derived from the request.
	result := "vm-cid-" + req.Context.RequestID
	var buf bytes.Buffer
	if err := EncodeSuccess(&buf, result, ""); err != nil {
		t.Fatalf("EncodeSuccess: %v", err)
	}

	// 3. Decode the response envelope back.
	raw := bytes.TrimRight(buf.Bytes(), "\n")
	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	// result should round-trip as a JSON string.
	wantResult := `"vm-cid-cpi-abc123"`
	resultBytes, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if string(resultBytes) != wantResult {
		t.Errorf("result = %s, want %s", resultBytes, wantResult)
	}

	if resp.Error != nil {
		t.Errorf("Error = %v, want nil", resp.Error)
	}
	if resp.Log != "" {
		t.Errorf("Log = %q, want empty", resp.Log)
	}
}

// -----------------------------------------------------------------------
// Single-line output verification
// -----------------------------------------------------------------------

// TestOutputIsSingleLine verifies that encoder output does not contain
// embedded newlines (i.e. is not pretty-printed).
func TestOutputIsSingleLine(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		var buf bytes.Buffer
		if err := EncodeSuccess(&buf, map[string]any{"a": 1, "b": 2}, "log"); err != nil {
			t.Fatal(err)
		}
		line := strings.TrimRight(buf.String(), "\n")
		if strings.ContainsRune(line, '\n') {
			t.Errorf("success output contains embedded newline: %q", buf.String())
		}
	})

	t.Run("error", func(t *testing.T) {
		var buf bytes.Buffer
		if err := EncodeError(&buf, "Bosh::Clouds::CloudError", "msg", false, "log"); err != nil {
			t.Fatal(err)
		}
		line := strings.TrimRight(buf.String(), "\n")
		if strings.ContainsRune(line, '\n') {
			t.Errorf("error output contains embedded newline: %q", buf.String())
		}
	})
}
