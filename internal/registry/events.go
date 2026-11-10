package registry

import (
	"context"
	"fmt"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// EventTopology describes one message broker event topic and its producers and consumers.
type EventTopology struct {
	SymbolID  int64           `json:"symbol_id"`
	Broker    string          `json:"broker"`
	Topic     string          `json:"topic"`
	FQN       string          `json:"fqn"`
	Producers []EventEndpoint `json:"producers,omitempty"`
	Consumers []EventEndpoint `json:"consumers,omitempty"`
}

// EventEndpoint describes one code function/method producing or consuming an event.
type EventEndpoint struct {
	SymbolID   int64            `json:"symbol_id"`
	SymbolName string           `json:"symbol_name"`
	SymbolFQN  string           `json:"symbol_fqn"`
	FilePath   string           `json:"file_path"`
	Line       int              `json:"line"`
	Confidence model.Confidence `json:"confidence"`
	Detail     string           `json:"detail,omitempty"`
}

// UnifyEventSymbols collapses duplicate event topic symbols (same FQN) across files
// into a single canonical node, retargeting edges so that all publishers and consumers
// meet at the same graph node.
func (r *Registry) UnifyEventSymbols(ctx context.Context, runID int64) error {
	rows, err := r.db.QueryContext(ctx, `
		SELECT s.id, s.fqn
		  FROM symbols s
		  JOIN files f ON f.id = s.file_id
		 WHERE s.index_run_id = ? AND s.kind = ?
		 ORDER BY s.fqn, f.path, s.start_line, s.id`,
		runID, string(model.KindEvent))
	if err != nil {
		return fmt.Errorf("registry: query event symbols: %w", err)
	}
	defer func() { _ = rows.Close() }()

	canonical := make(map[string]int64)
	var rebinds [][2]int64
	var toDelete []int64

	for rows.Next() {
		var id int64
		var fqn string
		if err := rows.Scan(&id, &fqn); err != nil {
			return err
		}
		if canonID, ok := canonical[fqn]; ok {
			rebinds = append(rebinds, [2]int64{id, canonID})
			toDelete = append(toDelete, id)
		} else {
			canonical[fqn] = id
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(rebinds) == 0 {
		return nil
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	updateDst, err := tx.PrepareContext(ctx, `UPDATE edges SET dst_id = ? WHERE dst_id = ? AND index_run_id = ?`)
	if err != nil {
		return err
	}
	defer func() { _ = updateDst.Close() }()

	updateSrc, err := tx.PrepareContext(ctx, `UPDATE edges SET src_id = ? WHERE src_id = ? AND index_run_id = ?`)
	if err != nil {
		return err
	}
	defer func() { _ = updateSrc.Close() }()

	deleteSym, err := tx.PrepareContext(ctx, `DELETE FROM symbols WHERE id = ? AND index_run_id = ?`)
	if err != nil {
		return err
	}
	defer func() { _ = deleteSym.Close() }()

	for _, pair := range rebinds {
		dupID, canonID := pair[0], pair[1]
		if _, err := updateDst.ExecContext(ctx, canonID, dupID, runID); err != nil {
			return err
		}
		if _, err := updateSrc.ExecContext(ctx, canonID, dupID, runID); err != nil {
			return err
		}
	}
	for _, id := range toDelete {
		if _, err := deleteSym.ExecContext(ctx, id, runID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// EventTopologyReport lists all message broker topics and their producers and consumers.
func (r *Registry) EventTopologyReport(ctx context.Context, runID int64) ([]EventTopology, error) {
	// First gather all canonical event symbols
	rows, err := r.db.QueryContext(ctx, `
		SELECT s.id, s.name, s.fqn
		  FROM symbols s
		 WHERE s.index_run_id = ? AND s.kind = ?
		 ORDER BY s.fqn`,
		runID, string(model.KindEvent))
	if err != nil {
		return nil, fmt.Errorf("registry: read event symbols: %w", err)
	}
	defer func() { _ = rows.Close() }()

	topicsBySymID := make(map[int64]*EventTopology)
	var ordered []*EventTopology

	for rows.Next() {
		var id int64
		var name, fqn string
		if err := rows.Scan(&id, &name, &fqn); err != nil {
			return nil, err
		}
		broker := ""
		parts := strings.Split(fqn, ":")
		if len(parts) >= 3 {
			broker = parts[1]
		}
		et := &EventTopology{
			SymbolID: id,
			Broker:   broker,
			Topic:    name,
			FQN:      fqn,
		}
		topicsBySymID[id] = et
		ordered = append(ordered, et)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Query edges connecting to event symbols
	edgeRows, err := r.db.QueryContext(ctx, `
		SELECT e.kind, e.confidence, e.evidence_file, e.evidence_line, COALESCE(e.detail, ''),
		       src.id, src.name, src.fqn,
		       evt.id
		  FROM edges e
		  JOIN symbols evt ON evt.id = e.dst_id
		  JOIN symbols src ON src.id = e.src_id
		 WHERE e.index_run_id = ? AND evt.kind = ? AND e.kind IN (?, ?)
		 ORDER BY evt.fqn, e.kind, src.name`,
		runID, string(model.KindEvent), string(model.EdgePublishes), string(model.EdgeConsumes))
	if err != nil {
		return nil, fmt.Errorf("registry: read event edges: %w", err)
	}
	defer func() { _ = edgeRows.Close() }()

	for edgeRows.Next() {
		var (
			edgeKind, confStr, file, detail string
			line                            int
			srcID, evtID                    int64
			srcName, srcFQN                 string
		)
		if err := edgeRows.Scan(&edgeKind, &confStr, &file, &line, &detail, &srcID, &srcName, &srcFQN, &evtID); err != nil {
			return nil, err
		}
		et, ok := topicsBySymID[evtID]
		if !ok {
			continue
		}
		endpoint := EventEndpoint{
			SymbolID:   srcID,
			SymbolName: srcName,
			SymbolFQN:  srcFQN,
			FilePath:   file,
			Line:       line,
			Confidence: model.Confidence(confStr),
			Detail:     detail,
		}
		if edgeKind == string(model.EdgePublishes) {
			et.Producers = append(et.Producers, endpoint)
		} else if edgeKind == string(model.EdgeConsumes) {
			et.Consumers = append(et.Consumers, endpoint)
		}
	}
	if err := edgeRows.Err(); err != nil {
		return nil, err
	}

	result := make([]EventTopology, len(ordered))
	for i, et := range ordered {
		result[i] = *et
	}
	return result, nil
}
