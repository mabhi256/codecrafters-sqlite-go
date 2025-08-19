package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
)

type Table struct {
	ID        int
	Name      string
	RootPage  int64
	Columns   []Column
	RowsCount int
}

type Column struct {
	Name     string
	Type     string
	ValueStr string
}

const (
	DBFileHeaderSize    = 100
	BTreePageHeaderSize = 8
	PageSizeOffset      = 16
	CellCountOffset     = 3
	CellPointerOffset   = 8 // Assuming no interior pages

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

	tables, err := getTablesList(databaseFile, cellOffsets, pageSize)
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

	case strings.HasPrefix(command, "select count(*)"):
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

	case strings.HasPrefix(command, "select"):
		selectInfo, err := ParseSelect(command)
		if err != nil {
			log.Fatal(err)
		}

		table, err := getTableByName(selectInfo.TableName, tables)
		if err != nil {
			log.Fatal(err)
		}

		selectInfo.WhereValue = strings.ReplaceAll(selectInfo.WhereValue, "'", "")
		rows, err := table.getRows(databaseFile, pageSize, selectInfo)
		if err != nil {
			log.Fatal(err)
		}

		for _, row := range rows {
			fmt.Println(row)
		}
		fmt.Println()
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

func getSerialTypeContentSize(serialType int64) int64 {
	isEven := serialType%2 == 0

	if serialType >= 12 && isEven {
		return (serialType - 12) / 2
	} else if serialType >= 13 && serialType%2 != 0 {
		return (serialType - 13) / 2
	}

	switch serialType {
	case 0, 1, 2, 3, 4:
		return serialType
	case 5:
		return 6
	case 6, 7:
		return 8
	case 8, 9:
		return 0
	default:
		return -1 // serial type 10, 11 is reserved for internal use and will never appear in the db file.
	}
}

func bytesToInt(data []byte) int64 {
	var result int64
	for _, b := range data {
		result = result<<8 + int64(b)
	}
	return result
}

func serialTypeToInt(serialType int64, data []byte) (int64, error) {
	switch serialType {
	case 0:
		return 0, nil // NULL
	case 1:
		return int64(data[0]), nil // 8-bit (treat as unsigned for root pages)
	case 2:
		return int64(binary.BigEndian.Uint16(data)), nil // 16-bit
	case 3:
		// 24-bit - simple big-endian conversion
		return int64(data[0])<<16 | int64(data[1])<<8 | int64(data[2]), nil
	case 4:
		return int64(binary.BigEndian.Uint32(data)), nil // 32-bit
	case 5:
		// 48-bit - simple big-endian conversion
		val := int64(binary.BigEndian.Uint16(data[0:2]))<<32 | int64(binary.BigEndian.Uint32(data[2:6]))
		return val, nil
	case 6:
		return int64(binary.BigEndian.Uint64(data)), nil // 64-bit
	case 8:
		return 0, nil // Constant 0
	case 9:
		return 1, nil // Constant 1
	default:
		return 0, fmt.Errorf("unsupported serial type: %d", serialType)
	}
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

func getTablesList(dbFile *os.File, cellOffsets []uint16, pageSize uint16) ([]Table, error) {
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
		schemaTypeSer, err := readVarint(dbFile, &pos)
		if err != nil {
			return nil, err
		}
		schemaTypeSize := getSerialTypeContentSize(schemaTypeSer)

		// Serial type for sqlite_schema.name
		schemaNameSer, err := readVarint(dbFile, &pos)
		if err != nil {
			return nil, err
		}
		schemaNameSize := getSerialTypeContentSize(schemaNameSer)

		// Serial type for sqlite_schema.tbl_name
		schemaTableNameSer, err := readVarint(dbFile, &pos)
		if err != nil {
			return nil, err
		}
		schemaTableNameSize := getSerialTypeContentSize(schemaTableNameSer)

		// Serial type for sqlite_schema.rootpage
		schemaRoot, err := readVarint(dbFile, &pos)
		if err != nil {
			return nil, err
		}
		schemaRootSize := getSerialTypeContentSize(schemaRoot)

		// Serial type for sqlite_schema.sql
		schemaSqlSer, err := readVarint(dbFile, &pos)
		if err != nil {
			return nil, err
		}
		schemaSqlSize := getSerialTypeContentSize(schemaSqlSer)

		// get table name
		tableNameOffset := startOfRecord + recordHeaderSize + schemaTypeSize + schemaNameSize
		tableNameBytes := make([]byte, schemaTableNameSize)
		_, err = dbFile.ReadAt(tableNameBytes, tableNameOffset)
		if err != nil {
			return nil, err
		}
		table.Name = string(tableNameBytes)

		// get table rootpage
		schemaRootOffset := tableNameOffset + schemaTableNameSize
		rootPageBytes := make([]byte, schemaRootSize)
		_, err = dbFile.ReadAt(rootPageBytes, schemaRootOffset)
		if err != nil {
			return nil, err
		}

		rootPageValue := bytesToInt(rootPageBytes)
		table.RootPage = rootPageValue

		// get table sql
		schemaSqlOffset := schemaRootOffset + schemaRootSize
		schemaSqlBytes := make([]byte, schemaSqlSize)
		_, err = dbFile.ReadAt(schemaSqlBytes, schemaSqlOffset)
		if err != nil {
			return nil, err
		}

		if table.Name != "sqlite_sequence" {
			table.ParseCreateTable(string(schemaSqlBytes))
		}

		rowCount, err := table.getRowCount(dbFile, pageSize)
		if err != nil {
			return nil, err
		}
		table.RowsCount = int(rowCount)

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

// Assuming single page
func (t *Table) getRowCount(dbFile *os.File, pageSize uint16) (uint16, error) {
	header := make([]byte, BTreePageHeaderSize)

	// B-tree page header of a table starts at the RootPage offset
	_, err := dbFile.ReadAt(header, (t.RootPage-1)*int64(pageSize)) // RootPage is 1 indexed
	if err != nil {
		return 0, err
	}

	rowCount, err := getValueAtOffset(header, CellCountOffset, 2)
	if err != nil {
		return 0, err
	}

	return rowCount, nil
}

func (t *Table) getRows(dbFile *os.File, pageSize uint16, selectInfo *SelectInfo) ([]string, error) {
	cellPointerArray := make([]byte, 2*t.RowsCount)

	// B-tree page header of a table starts at the RootPage offset
	rootPageOffset := (t.RootPage - 1) * int64(pageSize) // RootPage is 1 indexed
	_, err := dbFile.ReadAt(cellPointerArray, rootPageOffset+CellPointerOffset)
	if err != nil {
		return nil, err
	}

	cellOffsets := make([]uint16, t.RowsCount)
	for i := range t.RowsCount {
		offset, err := getValueAtOffset(cellPointerArray, int(2*i), 2)
		if err != nil {
			return nil, err
		}
		cellOffsets[i] = offset
	}

	rows := []string{}

	for _, offset := range cellOffsets {
		pos := rootPageOffset + int64(offset)

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

		columnOffset := startOfRecord + recordHeaderSize
		// println(columnOffset)
		values := make([]string, len(selectInfo.Columns))
		var whereCondition bool

		for _, column := range t.Columns {
			// Serial type for sqlite_schema.type
			schemaTypeSer, err := readVarint(dbFile, &pos)
			if err != nil {
				return nil, err
			}
			schemaTypeSize := getSerialTypeContentSize(schemaTypeSer)
			// fmt.Printf("Column: %s, schemaType: %v, schemaTypeSize: %d\n", column.Name, schemaTypeSer, schemaTypeSize)

			valueBytes := make([]byte, schemaTypeSize)
			_, err = dbFile.ReadAt(valueBytes, columnOffset)
			if err != nil {
				return nil, err
			}

			if column.Type == "integer" {
				valueInt := bytesToInt(valueBytes)
				column.ValueStr = strconv.Itoa(int(valueInt))
			} else if column.Type == "text" {
				column.ValueStr = string(valueBytes)
			}

			if column.Name == selectInfo.WhereColumn {
				if strings.EqualFold(column.ValueStr, selectInfo.WhereValue) {
					whereCondition = true
				}
			}

			for i, columnName := range selectInfo.Columns {
				if column.Name == columnName {
					values[i] = column.ValueStr
					break
				}
			}
			columnOffset += schemaTypeSize
		}

		if selectInfo.WhereColumn == "" || whereCondition {
			rows = append(rows, strings.Join(values, "|"))
		}
	}

	return rows, nil
}
