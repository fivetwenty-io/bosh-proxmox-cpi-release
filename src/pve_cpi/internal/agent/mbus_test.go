package agent

import "testing"

func TestDeriveMBusFromBlobstore_Happy(t *testing.T) {
	t.Parallel()

	bs := BlobstoreSpec{
		Provider: "dav",
		Options: map[string]any{
			"endpoint": "https://10.0.0.1:25250",
		},
	}
	got := deriveMBusFromBlobstore(bs)
	want := "nats://10.0.0.1:4222"
	if got != want {
		t.Errorf("deriveMBusFromBlobstore = %q; want %q", got, want)
	}
}

func TestDeriveMBusFromBlobstore_PortPreservedNotInherited(t *testing.T) {
	t.Parallel()

	bs := BlobstoreSpec{
		Options: map[string]any{
			"endpoint": "http://director.example.com:8080/dav",
		},
	}
	got := deriveMBusFromBlobstore(bs)
	want := "nats://director.example.com:4222"
	if got != want {
		t.Errorf("deriveMBusFromBlobstore = %q; want %q", got, want)
	}
}

func TestDeriveMBusFromBlobstore_NilOptions(t *testing.T) {
	t.Parallel()

	got := deriveMBusFromBlobstore(BlobstoreSpec{Options: nil})
	if got != "" {
		t.Errorf("deriveMBusFromBlobstore(nil opts) = %q; want empty", got)
	}
}

func TestDeriveMBusFromBlobstore_MissingEndpoint(t *testing.T) {
	t.Parallel()

	bs := BlobstoreSpec{Options: map[string]any{"other": "x"}}
	if got := deriveMBusFromBlobstore(bs); got != "" {
		t.Errorf("deriveMBusFromBlobstore = %q; want empty", got)
	}
}

func TestDeriveMBusFromBlobstore_NonStringEndpoint(t *testing.T) {
	t.Parallel()

	bs := BlobstoreSpec{Options: map[string]any{"endpoint": 1234}}
	if got := deriveMBusFromBlobstore(bs); got != "" {
		t.Errorf("deriveMBusFromBlobstore(non-string) = %q; want empty", got)
	}
}

func TestDeriveMBusFromBlobstore_EmptyEndpoint(t *testing.T) {
	t.Parallel()

	bs := BlobstoreSpec{Options: map[string]any{"endpoint": ""}}
	if got := deriveMBusFromBlobstore(bs); got != "" {
		t.Errorf("deriveMBusFromBlobstore(empty) = %q; want empty", got)
	}
}

func TestDeriveMBusFromBlobstore_ParseError(t *testing.T) {
	t.Parallel()

	bs := BlobstoreSpec{Options: map[string]any{"endpoint": "://not a url"}}
	if got := deriveMBusFromBlobstore(bs); got != "" {
		t.Errorf("deriveMBusFromBlobstore(bad url) = %q; want empty", got)
	}
}

func TestDeriveMBusFromBlobstore_LoopbackRejected(t *testing.T) {
	t.Parallel()

	cases := []string{
		"https://127.0.0.1:25250",
		"http://localhost:25250",
		"http://LOCALHOST:25250",
		"http://[::1]:25250",
		"http://0.0.0.0:25250",
		// Anywhere in 127.0.0.0/8 — not just .1 — must also be rejected.
		"http://127.0.0.2:25250",
		"http://127.255.255.254:25250",
		// IPv6 unspecified address.
		"http://[::]:25250",
		// IPv4-mapped IPv6 loopback.
		"http://[::ffff:127.0.0.1]:25250",
	}
	for _, ep := range cases {
		bs := BlobstoreSpec{Options: map[string]any{"endpoint": ep}}
		if got := deriveMBusFromBlobstore(bs); got != "" {
			t.Errorf("endpoint=%q: deriveMBusFromBlobstore = %q; want empty", ep, got)
		}
	}
}

func TestDeriveMBusFromBlobstore_NoSchemeHostOnly(t *testing.T) {
	t.Parallel()

	// url.Parse on a bare host treats the value as a path; Hostname() is "".
	bs := BlobstoreSpec{Options: map[string]any{"endpoint": "10.0.0.1"}}
	if got := deriveMBusFromBlobstore(bs); got != "" {
		t.Errorf("deriveMBusFromBlobstore(no scheme) = %q; want empty (no host parsed)", got)
	}
}
