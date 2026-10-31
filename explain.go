package tracepanel

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// EXPLAIN for the console: the agent writes the statement (EXPLAIN, EXPLAIN ANALYZE) and sends it down the connection
// the traces go up; the app runs it on the database the query came from, by its alias, and answers with the rows,
// which the agent reads as a plan. Whatever EXPLAIN ANALYZE does is rolled back.

type explainRequest struct {
	SQL    string `json:"sql"`
	Params []any  `json:"params"`
	Vendor string `json:"vendor"`
	Alias  string `json:"alias"`
}

type explainAnswer struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

const explainTimeout = 15 * time.Second

// number is a JSON number as the query had it: an integer, or a float.
func number(value any) any {
	text, ok := value.(json.Number)
	if !ok {
		return value
	}
	if integer, err := text.Int64(); err == nil {
		return integer
	}
	if float, err := text.Float64(); err == nil {
		return float
	}
	return text.String()
}

func explain(request explainRequest) (*explainAnswer, error) {
	alias := request.Alias
	if alias == "" {
		alias = "default"
	}
	db := databaseNamed(alias)
	if db == nil {
		return nil, fmt.Errorf("the app has no database %s to run EXPLAIN on (open it with tracepanel.Driver)", alias)
	}
	ctx, cancel := context.WithTimeout(context.Background(), explainTimeout)
	defer cancel()
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	params := make([]any, len(request.Params))
	for i, value := range request.Params {
		params[i] = number(value)
	}
	rows, err := tx.QueryContext(ctx, request.SQL, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	answer := &explainAnswer{Columns: columns, Rows: [][]any{}}
	for rows.Next() {
		cells := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range cells {
			pointers[i] = &cells[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}
		for i, cell := range cells {
			if text, ok := cell.([]byte); ok {
				cells[i] = string(text)
			}
		}
		answer.Rows = append(answer.Rows, cells)
	}
	return answer, rows.Err()
}
