package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"strings"
	// Available if you need it!
	// "github.com/xwb1989/sqlparser"
)

type Table struct {
	ID       int
	Name     string
	RootPage int64
}

const (
	DBFileHeaderSize    = 100
	BTreePageHeaderSize = 8
	PageSizeOffset      = 16
	CellCountOffset     = 3
	CellPointerOffset   = 8

	VARINT_CONTINUATION_MASK = 0x80 // 0b1000_0000
	VARINT_VALUE_MASK        = 0x7F // 0b0111_1111
	MaxVarintBytes           = 9
)

// Usage: your_program.sh sample.db .dbinfo
func main() {
	databaseFilePath := os.Args[1]
	command := os.Args[2]

	databaseFile, err := os.Open(databaseFilePath)
	if err != nil {
		log.Fatal(err)
	}

	pageSize, err := getPageSize(databaseFile)
	if err != nil {
		log.Fatal(err)
	}

	cellCount, err := getCellCount(databaseFile)
	if err != nil {
		log.Fatal(err)
	}

	cellOffsets, err := getCellOffsets(databaseFile, cellCount)
	if err != nil {
		log.Fatal(err)
	}

	tables, err := getTablesList(databaseFile, cellOffsets)
	if err != nil {
		log.Fatal(err)
	}

	command = strings.TrimSpace(command)
	command = strings.ToLower(command)

	switch {
	case command == ".dbinfo":
		// fmt.Fprintln(os.Stderr, "Logs from your program will appear here!")
		fmt.Printf("database page size: %v\n", pageSize)
		fmt.Printf("number of tables: %v\n", cellCount)

	case command == ".tables":
		for _, table := range tables {
			fmt.Printf("%s ", table.Name)
		}
		fmt.Println()

	case strings.HasPrefix(command, "select"):
		parts := strings.Fields(command)

		table, err := getTableByName(parts[len(parts)-1], tables)
		if err != nil {
			log.Fatal(err)
		}

		rowCount, err := table.getRowCount(databaseFile, pageSize)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(rowCount)
	default:
		fmt.Println("Unknown command", command)
		os.Exit(1)
	}
}

func getValueAtOffset(header []byte, offset, size int) (uint16, error) {
	var value uint16
	valueRaw := bytes.NewReader(header[offset : offset+size])

	err := binary.Read(valueRaw, binary.BigEndian, &value)
	if err != nil {
		return 0, err
	}

	return value, nil
}

func getPageSize(dbFile *os.File) (uint16, error) {
	header := make([]byte, DBFileHeaderSize)

	_, err := dbFile.Read(header)
	if err != nil {
		return 0, err
	}

	pageSize, err := getValueAtOffset(header, PageSizeOffset, 2)
	if err != nil {
		return 0, err
	}

	return pageSize, nil
}

func getCellCount(dbFile *os.File) (uint16, error) {
	header := make([]byte, BTreePageHeaderSize)

	// B-tree page header in the 1st page starts after DB file header
	_, err := dbFile.ReadAt(header, DBFileHeaderSize)
	if err != nil {
		return 0, err
	}

	cellCount, err := getValueAtOffset(header, CellCountOffset, 2)
	if err != nil {
		return 0, err
	}

	return cellCount, nil
}

func readVarint(dbFile *os.File, pos *int64) (int64, error) {
	var num int64
	size := 1 // number of bytes used by varint
	b := make([]byte, 1)

	for {
		_, err := dbFile.ReadAt(b, *pos)
		if err != nil {
			return 0, fmt.Errorf("unable to read varint: %w", err)
		}

		currentByte := b[0]
		currentValue := currentByte & VARINT_VALUE_MASK
		num = num<<7 | int64(currentValue)

		//check continuation bit
		if (currentByte & VARINT_CONTINUATION_MASK) == VARINT_CONTINUATION_MASK {
			*pos++
			size++
		} else {
			break
		}
	}

	if size > MaxVarintBytes {
		return 0, fmt.Errorf("varint too long: %d bytes", size)
	}

	*pos++ // Point to next unread byte
	return num, nil
}

func decodeTextSize(serialType int64) int64 {
	if serialType >= 12 && serialType%2 == 0 {
		return (serialType - 12) / 2
	} else if serialType >= 13 && serialType%2 != 0 {
		return (serialType - 13) / 2
	}

	return 0
}

func getCellOffsets(dbFile *os.File, cellCount uint16) ([]uint16, error) {
	cellPointerArray := make([]byte, 2*cellCount)
	_, err := dbFile.ReadAt(cellPointerArray, DBFileHeaderSize+CellPointerOffset)
	if err != nil {
		return nil, err
	}

	cellOffsets := make([]uint16, cellCount)
	for i := range cellCount {
		offset, err := getValueAtOffset(cellPointerArray, int(2*i), 2)
		if err != nil {
			return nil, err
		}
		cellOffsets[i] = offset
	}

	return cellOffsets, nil
}

func getTablesList(dbFile *os.File, cellOffsets []uint16) ([]Table, error) {
	tables := []Table{}

	for i, offset := range cellOffsets {
		pos := int64(offset)
		table := Table{
			ID: i,
		}

		// Size of the record
		_, err := readVarint(dbFile, &pos)
		if err != nil {
			return nil, err
		}

		// rowId
		_, err = readVarint(dbFile, &pos)
		if err != nil {
			return nil, err
		}

		// Record header
		// Size of record header
		startOfRecord := pos
		recordHeaderSize, err := readVarint(dbFile, &pos)
		if err != nil {
			return nil, err
		}

		// Serial type for sqlite_schema.type
		schemaType, err := readVarint(dbFile, &pos)
		if err != nil {
			return nil, err
		}
		schemaTypeSize := decodeTextSize(schemaType)

		// Serial type for sqlite_schema.name
		schemaName, err := readVarint(dbFile, &pos)
		if err != nil {
			return nil, err
		}
		schemaNameSize := decodeTextSize(schemaName)

		// Serial type for sqlite_schema.tbl_name
		schemaTableName, err := readVarint(dbFile, &pos)
		if err != nil {
			return nil, err
		}
		schemaTableNameSize := decodeTextSize(schemaTableName)

		// Serial type for sqlite_schema.rootpage
		schemaRoot, err := readVarint(dbFile, &pos)
		if err != nil {
			return nil, err
		}
		table.RootPage = schemaRoot

		// // Serial type for sqlite_schema.sql
		// schemaSql, err := readVarint(databaseFile, &pos)
		// if err != nil {
		// 	log.Fatal(err)
		// }

		tableNameOffset := startOfRecord + recordHeaderSize + schemaTypeSize + schemaNameSize
		tableName := make([]byte, schemaTableNameSize)
		_, err = dbFile.ReadAt(tableName, tableNameOffset)
		if err != nil {
			return nil, err
		}
		table.Name = string(tableName)

		tables = append(tables, table)
	}

	return tables, nil
}

func getTableByName(name string, tables []Table) (*Table, error) {
	for _, table := range tables {
		if table.Name == name {
			return &table, nil
		}
	}

	return nil, fmt.Errorf("unable to find table: %s", name)
}

func (t *Table) getRowCount(dbFile *os.File, pageSize uint16) (uint16, error) {
	header := make([]byte, BTreePageHeaderSize)

	// B-tree page header of a table starts at the RootPage offset
	_, err := dbFile.ReadAt(header, t.RootPage*int64(pageSize))
	if err != nil {
		return 0, err
	}

	rowCount, err := getValueAtOffset(header, CellCountOffset, 2)
	if err != nil {
		return 0, err
	}

	return rowCount, nil
}
