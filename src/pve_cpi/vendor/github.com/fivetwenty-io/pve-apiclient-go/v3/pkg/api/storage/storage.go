package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/client"
	pveerr "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"
)

var errSizeGiBPositive = errors.New("sizeGiB must be > 0")

// Service defines storage-related helpers.
type Service interface {
	CreateVolume(ctx context.Context, node, storage string, sizeGiB int, format string, vmid int, name string) (string, error)
	// DeleteVolume issues DELETE on the volume and discards the returned UPID.
	// 404 is treated as success. WARNING: PVE's DELETE queues an asynchronous
	// `imgdel` task that runs under the per-storage lock; when that lock is
	// contended, the queued imgdel may run long after this call returns,
	// removing a subsequently-recreated volume. Callers that immediately
	// re-upload to the same name MUST use DeleteVolumeAsync (or
	// DeleteVolumeIfExistsAsync) and await the returned UPID via Tasks().
	DeleteVolume(ctx context.Context, node, storage, volume string) error
	// DeleteVolumeIfExists deletes the named volume and reports whether it
	// actually removed anything. Returns (false, nil) when the volume did
	// not exist; (true, nil) on successful deletion; (_, err) on any other
	// failure. Distinct from DeleteVolume (which swallows 404 silently) —
	// callers that need the existed signal should use this method instead.
	// Carries the same async-imgdel caveat as DeleteVolume; prefer
	// DeleteVolumeIfExistsAsync when a subsequent upload to the same name is
	// imminent.
	DeleteVolumeIfExists(ctx context.Context, node, storage, volume string) (existed bool, err error)
	// DeleteVolumeAsync issues DELETE and returns the imgdel task UPID so the
	// caller can await it via Tasks() before proceeding. 404 is treated as
	// success and returns ("", nil). An empty UPID with a nil error means PVE
	// completed the delete synchronously (uncommon — most storage plugins
	// queue an imgdel task).
	DeleteVolumeAsync(ctx context.Context, node, storage, volume string) (upid string, err error)
	// DeleteVolumeIfExistsAsync combines DeleteVolumeIfExists and
	// DeleteVolumeAsync: returns (existed, upid, err). On 404 returns
	// (false, "", nil); on success returns (true, <upid>, nil). Caller MUST
	// await upid via Tasks() if a subsequent operation depends on the volume
	// being gone (e.g. re-upload with the same filename).
	DeleteVolumeIfExistsAsync(ctx context.Context, node, storage, volume string) (existed bool, upid string, err error)
	Exists(ctx context.Context, node, storage, volume string) (bool, error)
	// Upload uploads a single file to the named storage pool as the given
	// content type (iso, import, vztmpl, ...). Returns the upload UPID; the
	// caller is responsible for awaiting it via Tasks() if synchronous
	// semantics are required.
	Upload(ctx context.Context, node, storage, content, filename string, body io.Reader) (upid string, err error)
}

type service struct{ c client.Client }

// New returns a new storage service.
//
//nolint:ireturn // Factory pattern - returns interface to encapsulate implementation and enable mocking
func New(c client.Client) Service { return &service{c: c} }

func (s *service) CreateVolume(ctx context.Context, node, storage string, sizeGiB int, format string, vmid int, name string) (string, error) {
	if sizeGiB <= 0 {
		return "", errSizeGiBPositive
	}
	// PVE schema for POST /nodes/{node}/storage/{storage}/content:
	//   - filename (required)
	//   - size: kilobytes with optional 'M' or 'G' suffix (required, string)
	//   - vmid (required)
	//   - format (optional)
	// "content" is NOT an accepted parameter; passing it triggers
	// "property is not defined in schema".
	params := map[string]interface{}{
		"size":     fmt.Sprintf("%dG", sizeGiB),
		"vmid":     vmid,
		"filename": name,
	}
	if format != "" {
		params["format"] = format
	}

	data, err := s.c.PostCtx(ctx, fmt.Sprintf("/nodes/%s/storage/%s/content", url.PathEscape(node), url.PathEscape(storage)), params)
	if err != nil {
		return "", fmt.Errorf("failed to create volume on storage %q node %q: %w", storage, node, err)
	}

	if m, ok := data.(map[string]interface{}); ok {
		if v, ok := m["volid"].(string); ok {
			return v, nil
		}
	}

	if vol, ok := data.(string); ok {
		return vol, nil
	}

	return "", nil
}
func (s *service) DeleteVolume(ctx context.Context, node, storage, volume string) error {
	_, err := s.DeleteVolumeAsync(ctx, node, storage, volume)

	return err
}

// DeleteVolumeAsync issues DELETE on the volume and returns the queued imgdel
// task UPID (when PVE returns one). 404 is treated as success and returns
// ("", nil). Storage plugins that delete synchronously return ("", nil) too.
// Callers that need to re-create a volume with the same name immediately after
// must await the returned UPID via Tasks() — otherwise the queued imgdel can
// run later (after another caller has uploaded the replacement) and silently
// remove it.
func (s *service) DeleteVolumeAsync(ctx context.Context, node, storage, volume string) (string, error) {
	data, err := s.c.DeleteCtx(ctx, fmt.Sprintf("/nodes/%s/storage/%s/content/%s", url.PathEscape(node), url.PathEscape(storage), url.PathEscape(volume)), nil)
	if err == nil {
		return upidFromDeleteResponse(data), nil
	}

	if pveerr.IsAPIError(err) {
		var ae *pveerr.APIError
		if errors.As(err, &ae) && ae.IsNotFound() {
			return "", nil
		}
	}

	return "", fmt.Errorf("failed to delete volume %q from storage %q on node %q: %w", volume, storage, node, err)
}
func (s *service) Exists(ctx context.Context, node, storage, volume string) (bool, error) {
	_, err := s.c.GetCtx(ctx, fmt.Sprintf("/nodes/%s/storage/%s/content/%s", url.PathEscape(node), url.PathEscape(storage), url.PathEscape(volume)), nil)
	if err == nil {
		return true, nil
	}

	if pveerr.IsAPIError(err) {
		var ae *pveerr.APIError
		if errors.As(err, &ae) && ae.IsNotFound() {
			return false, nil
		}
	}

	return false, fmt.Errorf("failed to check if volume %q exists on storage %q node %q: %w", volume, storage, node, err)
}

func (s *service) DeleteVolumeIfExists(ctx context.Context, node, storage, volume string) (bool, error) {
	existed, _, err := s.DeleteVolumeIfExistsAsync(ctx, node, storage, volume)

	return existed, err
}

// DeleteVolumeIfExistsAsync issues DELETE on the volume, returning the
// existence signal along with the queued imgdel UPID (when PVE returns one).
// 404 → (false, "", nil); success → (true, <upid|"">, nil); other failure →
// (false, "", err). See DeleteVolumeAsync for the await guidance — callers
// that re-upload to the same volume name must await UPID via Tasks() to avoid
// a queued imgdel removing the replacement.
func (s *service) DeleteVolumeIfExistsAsync(ctx context.Context, node, storage, volume string) (bool, string, error) {
	data, err := s.c.DeleteCtx(ctx, fmt.Sprintf("/nodes/%s/storage/%s/content/%s", url.PathEscape(node), url.PathEscape(storage), url.PathEscape(volume)), nil)
	if err == nil {
		return true, upidFromDeleteResponse(data), nil
	}

	if pveerr.IsAPIError(err) {
		var ae *pveerr.APIError
		if errors.As(err, &ae) && ae.IsNotFound() {
			return false, "", nil
		}
	}

	return false, "", fmt.Errorf("failed to delete volume %q from storage %q on node %q: %w", volume, storage, node, err)
}

// upidFromDeleteResponse extracts the imgdel task UPID from PVE's DELETE
// response. PVE returns either {"data": "UPID:..."} or {"data": null} for
// synchronous-delete plugins. Anything else collapses to "".
func upidFromDeleteResponse(data any) string {
	if s, ok := data.(string); ok {
		return s
	}

	return ""
}

func (s *service) Upload(ctx context.Context, node, storage, content, filename string, body io.Reader) (string, error) {
	// PVE upload semantics: `content` is a form field; the file part is named
	// `filename` whose filename attribute carries the destination name. Do NOT
	// also pass `filename` as a form field — PVE rejects (HTTP 400) when the
	// same multipart part name appears twice.
	fields := map[string]string{"content": content}

	resp, err := s.c.UploadCtx(ctx, fmt.Sprintf("/nodes/%s/storage/%s/upload", url.PathEscape(node), url.PathEscape(storage)), fields, "filename", filename, body)
	if err != nil {
		return "", fmt.Errorf("failed to upload %q to storage %q on node %q: %w", filename, storage, node, err)
	}

	if upid, ok := resp.Data.(string); ok {
		return upid, nil
	}

	if m, ok := resp.Data.(map[string]interface{}); ok {
		if v, ok := m["upid"].(string); ok {
			return v, nil
		}
	}

	return "", nil
}
