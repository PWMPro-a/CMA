package mysql

import (
	"strings"
	"testing"
)

func TestTranslateQuery(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		contains   []string
		notContain []string
		returnsID  bool
	}{
		{
			name:     "ignore",
			input:    "insert or ignore into sample(id) values (?)",
			contains: []string{"insert ignore into sample"},
		},
		{
			name:       "upsert returning",
			input:      `insert into sample(id, value) values (?, ?) on conflict(id) do update set value = excluded.value returning id`,
			contains:   []string{"on duplicate key update id = last_insert_id(id)", "value = values(value)"},
			notContain: []string{"on conflict", "returning"},
			returnsID:  true,
		},
		{
			name:     "json each and scalar functions",
			input:    `select max(max(value, other_value) - max(offset, 0), ?), min(value, ?) from sample where id in (select value from json_each(?))`,
			contains: []string{"greatest(greatest(value, other_value) - greatest(offset, 0), ?)", "least(value, ?)", "json_table"},
		},
		{
			name:       "aggregate max unchanged",
			input:      `select max(value) from sample`,
			contains:   []string{"max(value)"},
			notContain: []string{"greatest"},
		},
		{
			name:       "integer buckets",
			input:      `select (timestamp_ms / 60000) * 60000, cast((timestamp_ms - ?) / ? as integer) from usage_events`,
			contains:   []string{"(timestamp_ms div 60000) * 60000", "as signed"},
			notContain: []string{"timestamp_ms / 60000", "as integer"},
		},
		{
			name:       "materialized cte hint",
			input:      `with scoped as materialized (select * from sample) select * from scoped`,
			contains:   []string{"scoped as (select"},
			notContain: []string{"materialized"},
		},
		{
			name: "sqlite index clause with mysql optimizer hint",
			input: `select /*+ INDEX(e idx_usage_events_latest_request_auth_file) */ e.id
				from usage_events e indexed by idx_usage_events_latest_request_auth_file`,
			contains:   []string{"/*+ index(e idx_usage_events_latest_request_auth_file) */"},
			notContain: []string{"indexed by"},
		},
		{
			name:     "sqlite json type",
			input:    `select json_type(metadata_json, '$.routing') from sample`,
			contains: []string{"json_type(json_extract(metadata_json, '$.routing'))"},
		},
		{
			name:  "cte values",
			input: `with targets(a, b) as (values (?, ?), (?, ?)) select * from targets`,
			contains: []string{
				"as (select ?, ? union all select ?, ?)",
			},
			notContain: []string{"as (values"},
		},
		{
			name: "filtered json aggregation",
			input: `select json_group_array(distinct provider) filter (where provider <> '')
				from filtered_events`,
			contains:   []string{"group_concat(distinct case when provider <> '' then json_quote(provider) end)", "'[]'"},
			notContain: []string{"json_group_array", "filter (where"},
		},
		{
			name: "leading cte insert",
			input: `with candidates as (select id from source), ranked as (select id from candidates)
				insert into target(id) select id from ranked
				on conflict(id) do update set id = excluded.id`,
			contains:   []string{"insert into target(id) with candidates as", "select id from ranked", "on duplicate key update"},
			notContain: []string{"with candidates as (select id from source), ranked as (select id from candidates) insert"},
		},
		{
			name: "conditional upsert",
			input: `insert into latest(id, timestamp_ms) values (?, ?)
				on conflict(id) do update set timestamp_ms = excluded.timestamp_ms
				where excluded.timestamp_ms > latest.timestamp_ms`,
			contains: []string{
				"timestamp_ms = if((values(timestamp_ms) > latest.timestamp_ms), values(timestamp_ms), latest.timestamp_ms)",
			},
			notContain: []string{" where values(timestamp_ms)"},
		},
		{
			name: "update from",
			input: `update ledger as l set event_id = e.id, updated_at_ms = ?
				from events as e where l.event_hash = e.event_hash and e.id > ?`,
			contains: []string{
				"update ledger as l join events as e on 1 = 1 set l.event_id = e.id, l.updated_at_ms = ? where l.event_hash = e.event_hash",
			},
			notContain: []string{" from events"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actual, returnsID := translateQuery(test.input)
			if returnsID != test.returnsID {
				t.Fatalf("returnsID = %v, want %v", returnsID, test.returnsID)
			}
			lower := strings.ToLower(actual)
			for _, expected := range test.contains {
				if !strings.Contains(lower, strings.ToLower(expected)) {
					t.Fatalf("translated query missing %q:\n%s", expected, actual)
				}
			}
			for _, excluded := range test.notContain {
				if strings.Contains(lower, strings.ToLower(excluded)) {
					t.Fatalf("translated query unexpectedly contains %q:\n%s", excluded, actual)
				}
			}
		})
	}
}
