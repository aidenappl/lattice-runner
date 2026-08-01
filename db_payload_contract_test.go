package main

import "testing"

// The runner half of the db_* payload contract.
//
// lattice-api pins the key names it sends (socket/db_payload_contract_test.go);
// this pins the key names this side reads, including the legacy spellings kept
// for compatibility. Ten defects shipped because the two sides disagreed and
// nothing compared them: an unknown key yields the zero value, so a mismatch
// produced an empty destination type or an empty object name rather than an
// error.
//
// If a test here fails, the orchestrator changed a wire key — fix both sides.

func TestPayloadStringAcceptsCanonicalAndLegacyKeys(t *testing.T) {
	tests := []struct {
		name    string
		payload map[string]any
		keys    []string
		want    string
	}{
		{
			name:    "canonical filename is read for the object name",
			payload: map[string]any{"filename": "db_test_20260730T000000Z.sql"},
			keys:    []string{"remote_path", "filename"},
			want:    "db_test_20260730T000000Z.sql",
		},
		{
			name:    "legacy remote_path still wins when present",
			payload: map[string]any{"remote_path": "legacy/key.sql", "filename": "new.sql"},
			keys:    []string{"remote_path", "filename"},
			want:    "legacy/key.sql",
		},
		{
			// This is the snapshot_id defect: the orchestrator sends a JSON
			// number, this side asserted .(string), got "", and every status
			// reply it echoed was dropped as unmatched.
			name:    "a JSON number is accepted where a string is expected",
			payload: map[string]any{"snapshot_id": float64(42)},
			keys:    []string{"snapshot_id"},
			want:    "42",
		},
		{
			name:    "restore falls back to snapshot_id because restore_id is never sent",
			payload: map[string]any{"snapshot_id": float64(7)},
			keys:    []string{"restore_id", "snapshot_id"},
			want:    "7",
		},
		{
			name:    "an empty string does not shadow a later key",
			payload: map[string]any{"remote_path": "", "filename": "real.sql"},
			keys:    []string{"remote_path", "filename"},
			want:    "real.sql",
		},
		{
			name:    "absent keys yield empty",
			payload: map[string]any{},
			keys:    []string{"remote_path", "filename"},
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := payloadString(tt.payload, tt.keys...); got != tt.want {
				t.Errorf("payloadString() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBackupDestinationFromAcceptsBothShapes(t *testing.T) {
	t.Run("nested backup_destination, which is what the API sends", func(t *testing.T) {
		p := map[string]any{
			"backup_destination": map[string]any{
				"type":   "s3",
				"config": map[string]any{"bucket": "backups"},
			},
		}
		gotType, gotConfig := backupDestinationFrom(p)
		if gotType != "s3" {
			t.Errorf("type = %q, want \"s3\" — this is the mismatch that made every manual snapshot fail at NewDestination(\"\")", gotType)
		}
		if gotConfig["bucket"] != "backups" {
			t.Errorf("config = %v, want bucket=backups", gotConfig)
		}
	})

	t.Run("flat dest_type/dest_config still works", func(t *testing.T) {
		p := map[string]any{
			"dest_type":   "samba",
			"dest_config": map[string]any{"share": "backups"},
		}
		gotType, gotConfig := backupDestinationFrom(p)
		if gotType != "samba" || gotConfig["share"] != "backups" {
			t.Errorf("flat shape not honoured: type=%q config=%v", gotType, gotConfig)
		}
	})

	t.Run("missing destination yields empty, not a panic", func(t *testing.T) {
		gotType, gotConfig := backupDestinationFrom(map[string]any{})
		if gotType != "" || gotConfig != nil {
			t.Errorf("expected empty destination, got type=%q config=%v", gotType, gotConfig)
		}
	})

	t.Run("a non-map backup_destination is ignored rather than fatal", func(t *testing.T) {
		gotType, _ := backupDestinationFrom(map[string]any{"backup_destination": "s3"})
		if gotType != "" {
			t.Errorf("expected empty type from malformed payload, got %q", gotType)
		}
	})
}

// TestScheduleAcceptsBackupDestinationKey is the direct regression test for the
// defect that meant no scheduled snapshot ever ran: the API sends the schedule's
// destination under "backup_destination", this side read "backup_dest".
func TestScheduleAcceptsBackupDestinationKey(t *testing.T) {
	canonical := map[string]any{
		"backup_destination": map[string]any{"type": "s3", "config": map[string]any{}},
	}
	legacy := map[string]any{
		"backup_dest": map[string]any{"type": "s3", "config": map[string]any{}},
	}

	for name, payload := range map[string]map[string]any{"canonical": canonical, "legacy": legacy} {
		dest, _ := payload["backup_destination"].(map[string]any)
		if dest == nil {
			dest, _ = payload["backup_dest"].(map[string]any)
		}
		if dest == nil {
			t.Errorf("%s: schedule destination not resolved — a nil destination makes handleScheduledSnapshot a silent no-op", name)
		}
	}
}
