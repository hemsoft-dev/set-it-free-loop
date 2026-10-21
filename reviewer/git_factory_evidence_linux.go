//go:build linux

package reviewer

import (
	"context"
	"strings"
)

// The factory calls this only after the acquisition's broker/unit stop and
// generation release. It reconstructs inline anchors without executing Git or
// trusting the worker's patch text as its own line-eligibility authority.
func captureGitMaterializationEvidence(ctx context.Context, m *gitMaterialization) (*SnapshotEvidence, error) {
	if ctx == nil || ctx.Err() != nil || m == nil {
		return nil, errGitMaterialization
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errGitMaterialization
	}
	binding, packet := m.manifest.Binding, m.packet
	p, err := validateGitMaterialization(m.manifest, []byte(packet), binding)
	m.mu.Unlock()
	if err != nil {
		return nil, errGitMaterialization
	}
	inventory := append([]snapshotFile(nil), p.Files...)
	for i := range inventory {
		inventory[i].Patch = ""
	}
	return restoreSnapshotPacket(ctx, m, binding.Snapshot, packet, inventory, func(id string, remaining *int) ([]byte, error) {
		if strings.Trim(id, "0") == "" {
			return nil, nil
		}
		data, err := m.blob(ctx, id, *remaining)
		if err == nil {
			*remaining -= len(data)
		}
		return data, err
	})
}
