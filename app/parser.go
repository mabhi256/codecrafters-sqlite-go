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
	// remove double quotes in create statement
	command = strings.ReplaceAll(command, "\"", "")
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
			colName := col.Name.String()
			t.Columns = append(t.Columns, Column{
				Name:         colName,
				Type:         col.Type.Type,
				IsPrimaryKey: col.Type.KeyOpt == 1, // ColumnKeyOption value colKeyPrimary
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
	IsCount       bool
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

	// Get columns - handle SELECT COUNT(*), SELECT *, and SELECT col1, col2, ...
	for _, expr := range sel.SelectExprs {
		switch e := expr.(type) {
		case *sqlparser.AliasedExpr:
			if funcExpr, ok := e.Expr.(*sqlparser.FuncExpr); ok &&
				strings.ToLower(funcExpr.Name.String()) == "count" {
				// Check if it's COUNT(*)
				selectInfo.IsCount = true
				selectInfo.Columns = append(selectInfo.Columns, "count(*)")
			} else {
				// Explicit column: SELECT col1, col2, ...
				colName := sqlparser.String(e.Expr)
				selectInfo.Columns = append(selectInfo.Columns, colName)
			}
		case *sqlparser.StarExpr:
			// Handle SELECT *
			selectInfo.Columns = append(selectInfo.Columns, "*")
		default:
			// Handle other expression types
			colName := sqlparser.String(e)
			selectInfo.Columns = append(selectInfo.Columns, colName)
		}
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
