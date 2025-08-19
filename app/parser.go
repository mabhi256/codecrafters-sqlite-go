package main

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/xwb1989/sqlparser"
)

func convertSQLiteToMySQL(sql string) string {
	// Convert AUTOINCREMENT to AUTO_INCREMENT
	sql = regexp.MustCompile(`(?i)\bautoincrement\b`).ReplaceAllString(sql, "")

	// Normalize whitespace
	sql = regexp.MustCompile(`\s+`).ReplaceAllString(sql, " ")
	sql = strings.TrimSpace(sql)

	return sql
}

func (t *Table) ParseCreateTable(command string) error {
	command = convertSQLiteToMySQL(command)
	stmt, err := sqlparser.Parse(command)
	if err != nil {
		fmt.Println("unable to parse sql query: %w", &err)
	}

	ddl, ok := stmt.(*sqlparser.DDL)
	if !ok || ddl.Action != sqlparser.CreateStr {
		return fmt.Errorf("not a CREATE TABLE statement")
	}

	if ddl.TableSpec != nil {
		for _, col := range ddl.TableSpec.Columns {
			t.Columns = append(t.Columns, Column{
				Name: col.Name.String(),
				Type: col.Type.Type,
			})
		}
	}

	return nil
}

type SelectInfo struct {
	Columns       []string
	TableName     string
	WhereColumn   string
	WhereValue    string
	WhereOperator string
}

func ParseSelect(command string) (*SelectInfo, error) {
	stmt, err := sqlparser.Parse(command)
	if err != nil {
		fmt.Println("unable to parse sql query: %w", &err)
	}

	sel, ok := stmt.(*sqlparser.Select)
	if !ok {
		return nil, fmt.Errorf("not a SELECT statement")
	}
	selectInfo := &SelectInfo{}

	// Get columns (assume explicit columns only, no *)
	for _, expr := range sel.SelectExprs {
		aliasedExpr := expr.(*sqlparser.AliasedExpr)
		colName := sqlparser.String(aliasedExpr.Expr)
		selectInfo.Columns = append(selectInfo.Columns, colName)
	}

	// Get table name (assume single table query)
	aliasedTable := sel.From[0].(*sqlparser.AliasedTableExpr)
	tableName := aliasedTable.Expr.(sqlparser.TableName)
	selectInfo.TableName = tableName.Name.String()

	// Get WHERE clause (assume single where clause with =)
	if sel.Where != nil {
		if comparisonExpr, ok := sel.Where.Expr.(*sqlparser.ComparisonExpr); ok {
			selectInfo.WhereColumn = sqlparser.String(comparisonExpr.Left) // Left side is column
			selectInfo.WhereOperator = comparisonExpr.Operator
			selectInfo.WhereValue = sqlparser.String(comparisonExpr.Right) // Right side is value
		}
	}

	return selectInfo, nil
}
