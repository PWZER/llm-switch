package store

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestMigrationProtocolKeyedChannels plants a pre-0005 schema (0001+0002+0003
// applied by hand) with legacy channels — an openai channel carrying
// responses_path, a duplicate same-protocol channel, explicit default
// auth styles, and route pins on both a surviving and a dropped channel —
// then runs Migrate() and checks the split/dedupe/pin-rewrite.
func TestMigrationProtocolKeyedChannels(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for _, name := range []string{"0001_init.sql", "0002_provider_scoped_models.sql", "0003_request_payloads.sql"} {
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := db.Write.Exec(string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	if _, err := db.Write.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	// Pretend 1-4 are applied so Migrate() runs only 0005.
	for v := 1; v <= 4; v++ {
		if _, err := db.Write.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, 1)`, v); err != nil {
			t.Fatalf("record version %d: %v", v, err)
		}
	}

	stmts := []string{
		`INSERT INTO providers (id, name, enabled, created_at, updated_at) VALUES (1, 'p1', 1, 1, 1)`,
		`INSERT INTO providers (id, name, enabled, created_at, updated_at) VALUES (2, 'p2', 1, 1, 1)`,
		// id 1: openai with responses_path -> splits off a responses row.
		`INSERT INTO channels (id, provider_id, name, protocol, base_url, chat_path, auth_style,
			responses_path, extra_headers, enabled, priority, weight,
			supports_embeddings, passthrough, force_upstream_stream, created_at, updated_at)
			VALUES (1, 1, 'main-openai', 'openai', 'https://a.example', '/chat/completions', 'bearer',
			'/responses', '{}', 1, 10, 1, 0, 1, 0, 1, 1)`,
		// id 2: duplicate openai channel on the same provider -> dropped (lowest id wins).
		`INSERT INTO channels (id, provider_id, name, protocol, base_url, chat_path, auth_style,
			responses_path, extra_headers, enabled, priority, weight,
			supports_embeddings, passthrough, force_upstream_stream, created_at, updated_at)
			VALUES (2, 1, 'dup-openai', 'openai', 'https://b.example', '/chat/completions', 'bearer',
			NULL, '{}', 1, 5, 1, 0, 1, 0, 1, 1)`,
		// id 3: anthropic with the protocol-default auth style -> normalizes to ''.
		`INSERT INTO channels (id, provider_id, name, protocol, base_url, chat_path, auth_style,
			responses_path, extra_headers, enabled, priority, weight,
			supports_embeddings, passthrough, force_upstream_stream, created_at, updated_at)
			VALUES (3, 1, 'main-anthropic', 'anthropic', 'https://c.example', '/v1/messages', 'x-api-key',
			NULL, '{}', 1, 10, 1, 0, 1, 0, 1, 1)`,
		// id 4: explicit non-default auth style survives verbatim.
		`INSERT INTO channels (id, provider_id, name, protocol, base_url, chat_path, auth_style,
			responses_path, extra_headers, enabled, priority, weight,
			supports_embeddings, passthrough, force_upstream_stream, created_at, updated_at)
			VALUES (4, 2, 'p2-anthropic', 'anthropic', 'https://d.example', '/v1/messages', 'bearer',
			NULL, '{}', 1, 10, 1, 0, 1, 0, 1, 1)`,
		// Route pins: channel 2 (dropped) must degrade to provider-scoped,
		// channel 1 (survivor) must stay pinned.
		`INSERT INTO model_routes (name, targets_json, updated_at) VALUES ('r1',
			'[{"provider_id":1,"channel_id":2,"account_id":null,"upstream_model":"m"},
			  {"provider_id":1,"channel_id":1,"account_id":null,"upstream_model":"m"}]', 1)`,
		// A log row to carry through the channel_name rename (0005) and the
		// channel_protocol drop (0006).
		`INSERT INTO request_logs (ts, model, protocol_in, protocol_out, status, channel_name)
			VALUES (1, 'm', 'openai', 'openai', 200, 'main-openai')`,
	}
	for _, q := range stmts {
		if _, err := db.Write.Exec(q); err != nil {
			t.Fatalf("seed row: %v\n%s", err, q)
		}
	}

	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate to 5: %v", err)
	}

	// Channel set: p1 keeps openai id 1 (unchanged path), gains a responses
	// row with chat_path /responses, keeps anthropic id 3 with auth_style
	// normalized to ''; p2 keeps its explicit bearer anthropic row.
	type chRow struct {
		id        int64
		proto     string
		chatPath  string
		authStyle string
	}
	rows, err := db.Read.Query(`SELECT id, provider_id, protocol, chat_path, auth_style FROM channels ORDER BY id`)
	if err != nil {
		t.Fatalf("list channels: %v", err)
	}
	defer rows.Close()
	var got []chRow
	var provID int64
	for rows.Next() {
		var c chRow
		if err := rows.Scan(&c.id, &provID, &c.proto, &c.chatPath, &c.authStyle); err != nil {
			t.Fatalf("scan channel: %v", err)
		}
		got = append(got, c)
	}
	if len(got) != 4 {
		t.Fatalf("want 4 channels after split+dedupe, got %d", len(got))
	}
	// id 1 stays the openai row.
	if got[0].id != 1 || got[0].proto != "openai" || got[0].chatPath != "/chat/completions" || got[0].authStyle != "" {
		t.Fatalf("openai row mangled: %+v", got[0])
	}
	// id 3 survives as anthropic with normalized auth.
	if got[1].id != 3 || got[1].proto != "anthropic" || got[1].authStyle != "" {
		t.Fatalf("anthropic row mangled: %+v", got[1])
	}
	// id 4 keeps its explicit bearer.
	if got[2].id != 4 || got[2].proto != "anthropic" || got[2].authStyle != "bearer" {
		t.Fatalf("explicit auth_style lost: %+v", got[2])
	}
	// The spawned responses row gets a fresh id and the responses_path as chat_path.
	if got[3].proto != "responses" || got[3].chatPath != "/responses" || got[3].authStyle != "" {
		t.Fatalf("responses split row wrong: %+v", got[3])
	}

	// Route pin on the dropped channel degrades to provider-scoped; the pin
	// on the survivor is untouched.
	var targets string
	if err := db.Read.QueryRow(`SELECT targets_json FROM model_routes WHERE name = 'r1'`).Scan(&targets); err != nil {
		t.Fatalf("read route: %v", err)
	}
	compact := strings.ReplaceAll(strings.ReplaceAll(targets, " ", ""), "\n", "")
	if !strings.Contains(compact, `"channel_id":null`) {
		t.Fatalf("dropped-channel pin not degraded: %s", targets)
	}
	if !strings.Contains(compact, `"channel_id":1`) {
		t.Fatalf("surviving-channel pin lost: %s", targets)
	}

	// The log row survives the rename (0005) + drop (0006) of the redundant
	// channel column; channel_protocol must be gone from the schema.
	var protoIn string
	if err := db.Read.QueryRow(`SELECT protocol_in FROM request_logs WHERE ts = 1`).Scan(&protoIn); err != nil {
		t.Fatalf("log row lost: %v", err)
	}
	var cnt int
	if err := db.Read.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('request_logs') WHERE name IN ('channel_name', 'channel_protocol')`).
		Scan(&cnt); err != nil || cnt != 0 {
		t.Fatalf("channel column must be gone from request_logs (cnt=%d, err=%v)", cnt, err)
	}

	// The UNIQUE(provider_id, protocol) constraint is live.
	if _, err := db.Write.Exec(`
		INSERT INTO channels (provider_id, protocol, base_url, chat_path, auth_style,
			extra_headers, enabled, supports_embeddings, created_at, updated_at)
		VALUES (1, 'openai', 'https://x.example', '/chat/completions', '', '{}', 1, 0, 1, 1)`); err == nil {
		t.Fatalf("duplicate (provider, protocol) insert must fail")
	}
}
