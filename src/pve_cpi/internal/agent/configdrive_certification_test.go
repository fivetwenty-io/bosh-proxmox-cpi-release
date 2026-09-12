package agent

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCertificationProxyUsesProductionISOName(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("content", "iso"); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("filename", configDriveISOFilename(123))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("ISO bytes")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(map[string]string{"content_type": writer.FormDataContentType(), "body": base64.StdEncoding.EncodeToString(body.Bytes())})
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	const script = `import base64,json,pathlib,sys
sys.path.insert(0,str(pathlib.Path(sys.argv[1])/"scripts"))
from _storage_placement_vm_faults import JournalVMFaultProxy,request_fields
value=json.load(sys.stdin)
fields=request_fields(value["content_type"],base64.b64decode(value["body"]))
step={"kind":"vm.Storage.Upload","target":{"node":"n1","vmid":123,"storage":"e1"}}
assert JournalVMFaultProxy.matches({},step,"POST","/nodes/n1/storage/e1/upload",fields)
fields["filename"]="vm-999-config.iso"
assert not JournalVMFaultProxy.matches({},step,"POST","/nodes/n1/storage/e1/upload",fields)
`
	command := exec.CommandContext(t.Context(), "python3", "-c", script, root)
	command.Stdin = bytes.NewReader(input)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("production ISO proxy seam: %v: %s", err, output)
	}
}
