package main

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
)

type DBContext struct {
	DBFile   *os.File
	PageSize uint16
	Tables   []Table
}

type BTreePageData struct {
	PageHeader  []byte
	PageOffset  int64
	CellOffsets []uint16
	CellCount   uint16
}

type Table struct {
	ID       int
	Name     string
	RootPage int64
	Columns  []Column
}

type Column struct {
	Name         string
	Type         string
	IsPrimaryKey bool
}

const (
	DBFileHeaderSize            = 100
	BTreeLeafPageHeaderSize     = 8
	BTreeInteriorPageHeaderSize = 12
	PageSizeOffset              = 16
	CellCountOffset             = 3
	//CellPointerOffset   = 8 // Assuming no interior pages

	VARINT_CONTINUATION_MASK = 0x80 // 0b1000_0000
	VARINT_VALUE_MASK        = 0x7F // 0b0111_1111
	MaxVarintBytes           = 9
)

func initializeDB(dbFilePath string) (*DBContext, error) {
	dbFile, err := os.Open(dbFilePath)
	if err != nil {
		return nil, err
	}

	dbFileHeader, err := getDBFileHeader(dbFile)
	if err != nil {
		dbFile.Close()
		return nil, err
	}

	pageSize := getUint16AtOffset(dbFileHeader, PageSizeOffset)

	tables, err := processSchemaPage(dbFile, pageSize)
	if err != nil {
		dbFile.Close()
		return nil, err
	}

	return &DBContext{
		DBFile:   dbFile,
		PageSize: pageSize,
		Tables:   tables,
	}, nil
}

func getDBFileHeader(dbFile *os.File) ([]byte, error) {
	// Always read first 100 bytes from offset 0
	header := make([]byte, DBFileHeaderSize)

	_, err := dbFile.Read(header)
	if err != nil {
		return nil, err
	}

	return header, nil
}

func getBTreePageHeader(dbFile *os.File, pageNumber int64, pageSize uint16) ([]byte, error) {
	// Read the maximum possible header size (12 bytes)
	header := make([]byte, 12)
	offset := getPageOffset(pageNumber, pageSize)
	_, err := dbFile.ReadAt(header, offset)
	if err != nil {
		return nil, err
	}

	pageType := header[0]
	switch pageType {
	case 0x02, 0x05: // Interior index/table pages
		return header, nil // Return all 12 bytes
	case 0x0a, 0x0d: // Leaf index/table pages
		return header[:8], nil // Return only first 8 bytes
	default:
		return nil, fmt.Errorf("invalid page type: 0x%02x", pageType)
	}
}

func getPageOffset(pageNumber int64, pageSize uint16) int64 {
	var offset int64
	if pageNumber == 1 {
		offset = DBFileHeaderSize // Page 1 starts after DB header
	} else {
		offset = (pageNumber - 1) * int64(pageSize) // Other pages start after the previous page
	}

	return offset
}

// Assuming it will not overflow
func getUint16AtOffset(b []byte, offset uint16) uint16 {
	return binary.BigEndian.Uint16(b[offset : offset+2])
}

func getBTreePageData(dbFile *os.File, pageNumber int64, pageSize uint16) (*BTreePageData, error) {
	pageHeader, err := getBTreePageHeader(dbFile, pageNumber, pageSize)
	if err != nil {
		return nil, err
	}

	cellCount := getUint16AtOffset(pageHeader, CellCountOffset)
	pageOffset := getPageOffset(pageNumber, pageSize)
	cellPointerArrayOffset := pageOffset + int64(len(pageHeader))

	cellOffsets, err := getCellOffsets(dbFile, cellPointerArrayOffset, cellCount)
	if err != nil {
		return nil, err
	}

	return &BTreePageData{
		PageHeader:  pageHeader,
		PageOffset:  pageOffset,
		CellOffsets: cellOffsets,
		CellCount:   cellCount,
	}, nil
}

func getRowCount(dbFile *os.File, pageNumber int64, pageSize uint16) (int64, error) {
	pageHeader, err := getBTreePageHeader(dbFile, pageNumber, pageSize)
	if err != nil {
		return 0, err
	}

	cellCount := getUint16AtOffset(pageHeader, CellCountOffset)
	pageOffset := getPageOffset(pageNumber, pageSize)

	switch len(pageHeader) {
	case BTreeLeafPageHeaderSize:
		return int64(cellCount), nil
	case BTreeInteriorPageHeaderSize:
		cellPointerArrayOffset := pageOffset + int64(len(pageHeader))

		cellOffsets, err := getCellOffsets(dbFile, cellPointerArrayOffset, cellCount)
		if err != nil {
			return 0, err
		}

		var count int64 = 0
		for _, cellOffset := range cellOffsets {
			pos := pageOffset + int64(cellOffset)
			leftPointer := make([]byte, 4)
			_, err := dbFile.ReadAt(leftPointer, pos)
			if err != nil {
				return 0, err
			}

			leftPageNumber := binary.BigEndian.Uint32(leftPointer[:])
			leftCount, err := getRowCount(dbFile, int64(leftPageNumber), pageSize)
			if err != nil {
				return 0, err
			}
			count += leftCount
		}
		rightPageNumber := binary.BigEndian.Uint32(pageHeader[8:])
		rightCount, err := getRowCount(dbFile, int64(rightPageNumber), pageSize)
		if err != nil {
			return 0, err
		}
		count += rightCount

		return count, nil
	default:
		return 0, err
	}
}

func processSchemaPage(dbFile *os.File, pageSize uint16) ([]Table, error) {
	pageData, err := getBTreePageData(dbFile, 1, pageSize) // Schema is always on page 1
	if err != nil {
		return nil, err
	}

	tables, err := parseSchemaEntries(dbFile, pageData.CellOffsets)
	if err != nil {
		return nil, err
	}

	return tables, nil
}

func getRows(ctx *DBContext, selectInfo *SelectInfo, pageNumber int64, table *Table) ([]string, error) {
	pageData, err := getBTreePageData(ctx.DBFile, pageNumber, ctx.PageSize)
	if err != nil {
		log.Fatal(err)
	}

	cellCount := getUint16AtOffset(pageData.PageHeader, CellCountOffset)
	pageOffset := getPageOffset(pageNumber, ctx.PageSize)

	switch len(pageData.PageHeader) {
	case BTreeLeafPageHeaderSize:
		rows, err := table.fetchTableRows(ctx.DBFile, pageData, selectInfo)
		if err != nil {
			log.Fatal(err)
		}
		return rows, nil
	case BTreeInteriorPageHeaderSize:
		cellPointerArrayOffset := pageOffset + int64(len(pageData.PageHeader))

		cellOffsets, err := getCellOffsets(ctx.DBFile, cellPointerArrayOffset, cellCount)
		if err != nil {
			return nil, err
		}

		rows := []string{}
		for _, cellOffset := range cellOffsets {
			pos := pageOffset + int64(cellOffset)
			leftPointer := make([]byte, 4)
			_, err := ctx.DBFile.ReadAt(leftPointer, pos)
			if err != nil {
				return nil, err
			}
			// pos += 4
			// key, err := readVarint(dbFile, &pos) // will be used when reading select stmt rows
			// if err != nil {
			// 	return 0, err
			// }
			leftPageNumber := binary.BigEndian.Uint32(leftPointer[:])
			leftRows, err := getRows(ctx, selectInfo, int64(leftPageNumber), table)
			if err != nil {
				return nil, err
			}
			rows = append(rows, leftRows...)
		}
		rightPageNumber := binary.BigEndian.Uint32(pageData.PageHeader[8:])
		rightRows, err := getRows(ctx, selectInfo, int64(rightPageNumber), table)
		if err != nil {
			return nil, err
		}
		rows = append(rows, rightRows...)

		return rows, nil
	default:
		// Should not happen, BTree header can only be 8 or 12 bytes
		return nil, err
	}
}

func handleSelectRows(ctx *DBContext, selectInfo *SelectInfo) {
	table, err := getTableByName(selectInfo.TableName, ctx.Tables)
	if err != nil {
		log.Fatal(err)
	}

	rows, err := getRows(ctx, selectInfo, table.RootPage, table)
	if err != nil {
		log.Fatal(err)
	}

	for _, row := range rows {
		fmt.Println(row)
	}
}

func getCellOffsets(dbFile *os.File,
	cellPointerArrayOffset int64, cellCount uint16,
) (cellOffsets []uint16, err error) {
	cellPointerArray := make([]byte, 2*cellCount)
	_, err = dbFile.ReadAt(cellPointerArray, cellPointerArrayOffset)
	if err != nil {
		return nil, err
	}

	cellOffsets = make([]uint16, cellCount)
	for i := range cellCount {
		offset := getUint16AtOffset(cellPointerArray, 2*i)
		cellOffsets[i] = offset
	}

	return cellOffsets, nil
}

func readCell(dbFile *os.File,
	cellOffset int64, columns []Column, selectInfo *SelectInfo,
) (map[string]string, error) {
	pos := cellOffset

	// Cell structure: record size + rowid + record data
	_, err := readVarint(dbFile, &pos) // record size
	if err != nil {
		return nil, err
	}

	rowId, err := readVarint(dbFile, &pos)
	if err != nil {
		return nil, err
	}

	// Record structure: header size + column-wise serial types  + column-wise data
	startOfRecord := pos
	recordHeaderSize, err := readVarint(dbFile, &pos)
	if err != nil {
		return nil, err
	}

	// Read serial types for all columns and calculate offsets
	serialTypes := make([]int64, len(columns))
	columnOffsets := make([]int64, len(columns))
	currentOffset := startOfRecord + recordHeaderSize
	for i := range columns {
		serialType, err := readVarint(dbFile, &pos)
		if err != nil {
			return nil, err
		}
		serialTypes[i] = serialType
		columnOffsets[i] = currentOffset
		currentOffset += getContentSizeBySerialType(serialType)
	}

	// Add where-column to requested columns if not already present in select-columns
	// select col1, col2 from table1 where col3 = x;
	requestedColumns := selectInfo.Columns
	if selectInfo.WhereColumn != "" {
		found := slices.Contains(selectInfo.Columns, selectInfo.WhereColumn)
		if !found {
			requestedColumns = append(requestedColumns, selectInfo.WhereColumn)
		}
	}

	// Read requested columns
	result := make(map[string]string)
	for _, requestedCol := range requestedColumns {
		colIndex := -1
		for i, col := range columns {
			if col.Name == requestedCol {
				colIndex = i
				break
			}
		}
		if colIndex == -1 {
			continue
		}

		if serialTypes[colIndex] != 0 {
			value, err := readColumnValue(dbFile, columnOffsets[colIndex], serialTypes[colIndex])
			if err != nil {
				return nil, err
			}
			result[requestedCol] = value
		}
	}

	// Apply WHERE filter
	if selectInfo.WhereColumn != "" {
		if !strings.EqualFold(result[selectInfo.WhereColumn], selectInfo.WhereValue) {
			return nil, nil // filtered out - return empty map
		}
	}

	result["id"] = fmt.Sprintf("%d", rowId)

	return result, nil
}

func readVarint(dbFile *os.File, pos *int64) (int64, error) {
	var num int64
	b := make([]byte, 1) // reuse the same buffer

	for i := range MaxVarintBytes {
		_, err := dbFile.ReadAt(b, *pos)
		if err != nil {
			return 0, fmt.Errorf("unable to read varint: %w", err)
		}

		currentByte := b[0]
		var value byte
		if i == 9 {
			// all 8 bits of the ninth byte are used for reconstruction
			value = currentByte
		} else {
			// lower 7 bits of the first eight bytes are used for reconstruction
			value = currentByte & VARINT_VALUE_MASK
		}
		num = num<<7 | int64(value)
		*pos++

		// If continuation bit is not set, we're done
		if (currentByte & VARINT_CONTINUATION_MASK) == 0 {
			return num, nil
		}
	}

	return 0, fmt.Errorf("varint too long: %d bytes", MaxVarintBytes)
}

func readColumnValue(dbFile *os.File, offset int64, serialType int64) (string, error) {
	size := getContentSizeBySerialType(serialType)
	valueBytes := make([]byte, size)
	_, err := dbFile.ReadAt(valueBytes, offset)
	if err != nil {
		return "", err
	}

	value, err := serialTypeToValue(serialType, valueBytes)
	if err != nil {
		return "", err
	}

	return valueToString(value), nil
}

func getContentSizeBySerialType(serialType int64) int64 {
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

func serialTypeToValue(serialType int64, data []byte) (any, error) {
	switch serialType {
	case 0:
		return nil, nil // NULL
	case 1:
		// 8-bit signed integer
		return int8(data[0]), nil
	case 2:
		// 16-bit signed integer (big-endian)
		return int16(binary.BigEndian.Uint16(data)), nil
	case 3:
		// 24-bit signed integer (big-endian)
		value := int32(data[0])<<16 | int32(data[1])<<8 | int32(data[2])
		return (value << 8) >> 8, nil // Extend sign if negative
	case 4:
		// 32-bit signed integer (big-endian)
		return int32(binary.BigEndian.Uint32(data)), nil
	case 5:
		// 48-bit signed integer (big-endian)
		value := int64(binary.BigEndian.Uint16(data[0:2]))<<32 | int64(binary.BigEndian.Uint32(data[2:6]))
		return (value << 16) >> 16, nil // Extend sign if negative
	case 6:
		// 64-bit signed integer (big-endian)
		return int64(binary.BigEndian.Uint64(data)), nil
	case 7:
		// 64-bit IEEE floating point (big-endian)
		bits := binary.BigEndian.Uint64(data)
		return math.Float64frombits(bits), nil
	case 8:
		return int8(0), nil // Integer constant 0
	case 9:
		return int8(1), nil // Integer constant 1
	case 10, 11:
		return nil, fmt.Errorf("reserved serial types 10 and 11")
	default:
		if serialType >= 12 && serialType%2 == 0 {
			// BLOB: (N-12)/2 bytes
			return data, nil // Return as []byte
		} else if serialType >= 13 && serialType%2 == 1 {
			// TEXT: (N-13)/2 bytes (UTF-8)
			return string(data), nil
		}
		return nil, fmt.Errorf("unsupported serial type: %d", serialType)
	}
}

func valueToString(value any) string {
	if value == nil {
		return ""
	}

	switch v := value.(type) {
	case string:
		return v
	case []byte:
		return hex.EncodeToString(v)
	case int8, int16, int32, int64:
		return fmt.Sprintf("%d", v)
	case float64:
		return fmt.Sprintf("%g", v)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func parseSchemaEntries(dbFile *os.File, cellOffsets []uint16) ([]Table, error) {
	sqliteMasterColumns := []Column{
		{Name: "type", Type: "text"},
		{Name: "name", Type: "text"},
		{Name: "tbl_name", Type: "text"},
		{Name: "rootpage", Type: "integer"},
		{Name: "sql", Type: "text"},
	}

	schemaSelectInfo := &SelectInfo{
		Columns:     []string{"tbl_name", "rootpage", "sql"},
		WhereColumn: "type",
		WhereValue:  "table",
	}

	var tables []Table
	for i, offset := range cellOffsets {
		values, err := readCell(dbFile, int64(offset), sqliteMasterColumns, schemaSelectInfo)
		if err != nil {
			return nil, err
		}

		if len(values) == 0 { // filtered out
			continue
		}

		table := Table{
			ID:   i,
			Name: values["tbl_name"],
		}

		rootPageInt, _ := strconv.ParseInt(values["rootpage"], 10, 64)
		table.RootPage = rootPageInt

		if table.Name != "sqlite_sequence" {
			table.ParseCreateTable(values["sql"])
		}

		tables = append(tables, table)
	}

	return tables, nil
}

func (t *Table) fetchTableRows(dbFile *os.File, pageData *BTreePageData, selectInfo *SelectInfo) ([]string, error) {
	var rows []string

	for _, cellOffset := range pageData.CellOffsets {
		offset := pageData.PageOffset + int64(cellOffset)
		values, err := readCell(dbFile, offset, t.Columns, selectInfo)
		if err != nil {
			return nil, err
		}

		if len(values) == 0 { // filtered out by WHERE clause
			continue
		}

		// Build result string from requested columns
		resultValues := make([]string, len(selectInfo.Columns))
		for i, columnName := range selectInfo.Columns {
			resultValues[i] = values[columnName]
		}

		rows = append(rows, strings.Join(resultValues, "|"))
	}

	return rows, nil
}

func handleSelect(ctx *DBContext, command string) {
	selectInfo, err := ParseSelect(command)
	if err != nil {
		log.Fatal(err)
	}
	selectInfo.WhereValue = strings.ReplaceAll(selectInfo.WhereValue, "'", "")

	if selectInfo.IsCount {
		handleSelectCount(ctx, selectInfo)
	} else {
		handleSelectRows(ctx, selectInfo)
	}
}

func handleSelectCount(ctx *DBContext, selectInfo *SelectInfo) {
	table, err := getTableByName(selectInfo.TableName, ctx.Tables)
	if err != nil {
		log.Fatal(err)
	}

	rowCount, err := getRowCount(ctx.DBFile, table.RootPage, ctx.PageSize)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(rowCount)
}

func getTableByName(name string, tables []Table) (*Table, error) {
	for _, table := range tables {
		if table.Name == name {
			return &table, nil
		}
	}

	return nil, fmt.Errorf("unable to find table: %s", name)
}

// func getRowCount(table string) (uint16, error) {

// }

func logErr(err error) {
	if err != nil {
		log.Fatal(err)
		os.Exit(1)
	}
}

// Usage: your_program.sh sample.db .dbinfo
func main() {
	dbFilePath := os.Args[1]
	command := os.Args[2]

	ctx, err := initializeDB(dbFilePath)
	logErr(err)
	defer ctx.DBFile.Close()

	command = strings.TrimSpace(command)
	command = strings.ToLower(command)

	switch {
	case command == ".dbinfo":
		// fmt.Fprintln(os.Stderr, "Logs from your program will appear here!")
		fmt.Printf("database page size: %v\n", ctx.PageSize)
		fmt.Printf("number of tables: %v\n", len(ctx.Tables))

	case command == ".tables":
		for _, table := range ctx.Tables {
			fmt.Printf("%s ", table.Name)
		}

	case strings.HasPrefix(command, "select"):
		handleSelect(ctx, command)

	default:
		fmt.Println("Unknown command", command)
		os.Exit(1)
	}
}
