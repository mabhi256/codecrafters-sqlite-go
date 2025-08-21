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

const (
	DBFileHeaderSize            = 100
	BTreeLeafPageHeaderSize     = 8
	BTreeInteriorPageHeaderSize = 12
	PageSizeOffset              = 16
	ReservedSpaceOffset         = 20
	CellCountOffset             = 3

	VARINT_CONTINUATION_MASK = 0x80 // 0b1000_0000
	VARINT_VALUE_MASK        = 0x7F // 0b0111_1111
	MaxVarintBytes           = 9
)

type DBContext struct {
	DBFile      *os.File
	PageSize    int64
	UsableSpace int64
	Tables      []Table
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

type PageType uint8

const (
	InteriorIndexPage PageType = 0x02
	InteriorTablePage PageType = 0x05
	LeafIndexPage     PageType = 0x0a
	LeafTablePage     PageType = 0x0d
)

type Cell interface {
	GetData() map[string]any
}

type TableCell struct {
	RowID  int64
	Values map[string]string
}

func (c *TableCell) GetData() map[string]any {
	result := make(map[string]any)
	for k, v := range c.Values {
		result[k] = v
	}
	return result
}

// type IndexCell struct {
// 	Key    []byte
// 	RowIDs []int64 // For leaf index cells, might point to table rows
// }

// func (c *IndexCell) GetData() map[string]any {
// 	return map[string]any{
// 		"key":    c.Key,
// 		"rowids": c.RowIDs,
// 	}
// }

// func (c *IndexCell) GetRowID() int64 { return 0 } // Not applicable for index cells

type Page struct {
	PageNumber  int64
	PageOffset  int64
	PageType    PageType
	CellCount   int64
	CellOffsets []int64
	Context     *DBContext
	// Only for interior pages
	RightmostChild int64
}

func (p *Page) IsLeaf() bool {
	return p.PageType == LeafTablePage || p.PageType == LeafIndexPage
}

func (ctx *DBContext) readBytesAt(offset int64, size int64) ([]byte, error) {
	buffer := make([]byte, size)
	_, err := ctx.DBFile.ReadAt(buffer, offset)
	if err != nil {
		return nil, err
	}
	return buffer, nil
}

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

	pageSize := int64(getUint16AtOffset(dbFileHeader, 16))
	reservedSpace := int64(dbFileHeader[ReservedSpaceOffset])
	usableSpace := pageSize - reservedSpace

	ctx := &DBContext{
		DBFile:      dbFile,
		PageSize:    pageSize,
		UsableSpace: usableSpace,
	}

	// Parse schema from page 1 - Schema is always on page 1
	page, err := LoadPage(ctx, 1)
	if err != nil {
		return nil, err
	}

	tables, err := parseSchemaEntries(ctx, page.CellOffsets)
	if err != nil {
		return nil, err
	}
	ctx.Tables = tables

	return ctx, nil
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

func getPageOffset(pageNumber int64, pageSize int64) int64 {
	var offset int64
	if pageNumber == 1 {
		offset = DBFileHeaderSize // Page 1 starts after DB header
	} else {
		offset = (pageNumber - 1) * pageSize // Other pages start after the previous page
	}

	return offset
}

// Assuming it will not overflow
func getUint16AtOffset(b []byte, offset int64) uint16 {
	return binary.BigEndian.Uint16(b[offset : offset+2])
}

// LoadPage reads a page from the database file and parses its header
func LoadPage(ctx *DBContext, pageNumber int64) (*Page, error) {
	pageOffset := getPageOffset(pageNumber, ctx.PageSize)

	// Read page header - always read first 12 bytes (max header size)
	header, err := ctx.readBytesAt(pageOffset, 12)
	if err != nil {
		return nil, err
	}

	pageType := PageType(header[0])
	cellCount := int64(getUint16AtOffset(header, 3))

	page := &Page{
		PageNumber: pageNumber,
		PageOffset: pageOffset,
		PageType:   pageType,
		CellCount:  cellCount,
		Context:    ctx,
	}

	// Get header size and rightmost child for interior pages
	var headerSize int64
	switch pageType {
	case LeafTablePage, LeafIndexPage:
		headerSize = 8
	case InteriorTablePage, InteriorIndexPage:
		headerSize = 12
		page.RightmostChild = int64(binary.BigEndian.Uint32(header[8:12]))
	default:
		return nil, fmt.Errorf("unknown page type: 0x%02x", pageType)
	}

	// Read cell offsets
	cellPointerArrayOffset := pageOffset + headerSize
	cellOffsets, err := getCellOffsets(ctx, cellPointerArrayOffset, cellCount)
	if err != nil {
		return nil, err
	}
	page.CellOffsets = cellOffsets

	return page, nil
}

func getCellOffsets(ctx *DBContext, cellPointerArrayOffset int64, cellCount int64) ([]int64, error) {
	cellPointerArray, err := ctx.readBytesAt(cellPointerArrayOffset, 2*cellCount)
	if err != nil {
		return nil, err
	}

	cellOffsets := make([]int64, cellCount)
	for i := range cellCount {
		offset := int64(getUint16AtOffset(cellPointerArray, 2*i))
		cellOffsets[i] = offset
	}

	return cellOffsets, nil
}

func CountTableRows(ctx *DBContext, table *Table) (int64, error) {
	return countRowsInPage(ctx, table.RootPage)
}

func countRowsInPage(ctx *DBContext, pageNumber int64) (int64, error) {
	page, err := LoadPage(ctx, pageNumber)
	if err != nil {
		return 0, err
	}

	if page.IsLeaf() {
		return page.CellCount, nil
	}

	// Interior page - count rows in all children
	var totalRows int64

	// Count in left children (from cells)
	for _, cellOffset := range page.CellOffsets {
		pos := page.PageOffset + cellOffset
		leftPointer, err := page.Context.readBytesAt(pos, 4)
		if err != nil {
			return 0, err
		}

		leftPageNumber := int64(binary.BigEndian.Uint32(leftPointer))

		childRows, err := countRowsInPage(ctx, leftPageNumber)
		if err != nil {
			return 0, err
		}
		totalRows += childRows
	}

	// Count in rightmost child
	rightRows, err := countRowsInPage(ctx, page.RightmostChild)
	if err != nil {
		return 0, err
	}
	totalRows += rightRows

	return totalRows, nil
}

func CollectTableRows(ctx *DBContext, table *Table, selectInfo *SelectInfo) ([]string, error) {
	var rows []string
	err := collectRowsFromPage(ctx, table.RootPage, table, selectInfo, &rows)
	return rows, err
}

func collectRowsFromPage(ctx *DBContext, pageNumber int64, table *Table, selectInfo *SelectInfo, rows *[]string) error {
	page, err := LoadPage(ctx, pageNumber)
	if err != nil {
		return err
	}

	if page.IsLeaf() {
		// Read cells from leaf page
		cells, err := ReadTableCells(page, table, selectInfo)
		if err != nil {
			return err
		}

		for _, cell := range cells {
			if tableCell, ok := cell.(*TableCell); ok {
				resultValues := make([]string, len(selectInfo.Columns))
				for i, columnName := range selectInfo.Columns {
					resultValues[i] = tableCell.Values[columnName]
				}
				*rows = append(*rows, strings.Join(resultValues, "|"))
			}
		}
		return nil
	}

	// Interior page - process all children
	for _, cellOffset := range page.CellOffsets {
		pos := page.PageOffset + cellOffset
		leftPointer, err := page.Context.readBytesAt(pos, 4)
		if err != nil {
			return err
		}

		leftPageNumber := int64(binary.BigEndian.Uint32(leftPointer))

		if err := collectRowsFromPage(ctx, leftPageNumber, table, selectInfo, rows); err != nil {
			return err
		}
	}

	// Process rightmost child
	return collectRowsFromPage(ctx, page.RightmostChild, table, selectInfo, rows)
}

func ReadTableCells(page *Page, table *Table, selectInfo *SelectInfo) ([]Cell, error) {
	if page.PageType != LeafTablePage {
		return nil, fmt.Errorf("can only read table cells from table leaf pages")
	}

	var cells []Cell
	for _, cellOffset := range page.CellOffsets {
		offset := page.PageOffset + cellOffset
		values, err := readCell(page.Context, offset, table.Columns, selectInfo)
		if err != nil {
			return nil, err
		}

		if len(values) == 0 { // filtered out
			continue
		}

		// Extract row ID
		rowIDStr, exists := values["id"]
		if !exists {
			continue
		}
		rowID, _ := strconv.ParseInt(rowIDStr, 10, 64)

		cells = append(cells, &TableCell{
			RowID:  rowID,
			Values: values,
		})
	}

	return cells, nil
}

func readCell(ctx *DBContext, cellOffset int64, columns []Column, selectInfo *SelectInfo) (map[string]string, error) {
	// First, read enough bytes to get payload size and rowId (max 18 bytes for two varints)
	cellHeaderBuffer, err := ctx.readBytesAt(cellOffset, 18)
	if err != nil {
		return nil, err
	}

	// Cell structure: record size + rowid + record data
	pos := int64(0)
	P, err := readVarintFromBuffer(cellHeaderBuffer, &pos) // payload size
	if err != nil {
		return nil, err
	}

	rowId, err := readVarintFromBuffer(cellHeaderBuffer, &pos)
	if err != nil {
		return nil, err
	}

	// Calculate where payload data starts
	payloadStartOffset := cellOffset + pos

	// Read complete payload (handling overflow if necessary)
	completePayload, err := readCompletePayload(ctx, payloadStartOffset, P)
	if err != nil {
		return nil, err
	}

	// Now parse the record from complete payload
	return parseRecordFromPayload(completePayload, rowId, columns, selectInfo)
}

func readCompletePayload(ctx *DBContext, payloadStartOffset int64, P int64) ([]byte, error) {
	U := ctx.UsableSpace

	// Calculate overflow parameters
	X := U - 35 // Maximum payload without spilling onto an overflow page
	M := ((U - 12) * 32 / 255) - 23

	if P <= X {
		// No overflow - read all payload directly
		return ctx.readBytesAt(payloadStartOffset, P)
	}

	// Overflow case - need to read from multiple locations
	K := M + ((P - M) % (U - 4))

	var localPayloadSize int64
	if K <= X {
		localPayloadSize = K
	} else {
		localPayloadSize = M
	}

	// Read local payload portion
	localPayload, err := ctx.readBytesAt(payloadStartOffset, localPayloadSize)
	if err != nil {
		return nil, err
	}

	// Read overflow page number (4 bytes after local payload)
	overflowPageBytes, err := ctx.readBytesAt(payloadStartOffset+localPayloadSize, 4)
	if err != nil {
		return nil, err
	}
	overflowPageNum := int64(binary.BigEndian.Uint32(overflowPageBytes))

	// Read remaining payload from overflow pages
	remainingPayload, err := readOverflowPages(ctx, overflowPageNum, P-localPayloadSize)
	if err != nil {
		return nil, err
	}

	// Combine local and overflow payload
	return append(localPayload, remainingPayload...), nil
}

func readOverflowPages(ctx *DBContext, firstOverflowPage int64, remainingBytes int64) ([]byte, error) {
	var overflowPayload []byte
	currentPageNum := firstOverflowPage
	bytesLeftToRead := remainingBytes

	for currentPageNum != 0 && bytesLeftToRead > 0 {
		pageOffset := getPageOffset(currentPageNum, ctx.PageSize)

		// Read the 4-byte next page pointer first
		nextPageBytes, err := ctx.readBytesAt(pageOffset, 4)
		if err != nil {
			return nil, err
		}
		nextPageNum := int64(binary.BigEndian.Uint32(nextPageBytes))

		// Calculate how much payload data is available on this page
		maxOverflowPayload := ctx.UsableSpace - 4 // subtract 4 bytes for next page pointer
		payloadAvailable := min(bytesLeftToRead, maxOverflowPayload)

		// Read payload data from this overflow page
		pagePayload, err := ctx.readBytesAt(pageOffset+4, payloadAvailable)
		if err != nil {
			return nil, err
		}

		overflowPayload = append(overflowPayload, pagePayload...)
		bytesLeftToRead -= payloadAvailable
		currentPageNum = nextPageNum
	}

	if bytesLeftToRead > 0 {
		return nil, fmt.Errorf("incomplete overflow chain: still need %d bytes", bytesLeftToRead)
	}

	return overflowPayload, nil
}

func parseRecordFromPayload(payload []byte, rowId int64, columns []Column, selectInfo *SelectInfo) (map[string]string, error) {
	pos := int64(0)

	// Record structure: header size + column-wise serial types + column-wise data
	startOfRecord := pos
	recordHeaderSize, err := readVarintFromBuffer(payload, &pos)
	if err != nil {
		return nil, err
	}

	// Read serial types for all columns and calculate offsets
	serialTypes := make([]int64, len(columns))
	columnOffsets := make([]int64, len(columns))
	currentOffset := startOfRecord + recordHeaderSize
	for i := range columns {
		serialType, err := readVarintFromBuffer(payload, &pos)
		if err != nil {
			return nil, err
		}
		serialTypes[i] = serialType
		columnOffsets[i] = currentOffset
		currentOffset += getContentSizeBySerialType(serialType)
	}

	// Add where-column to requested columns if not already present in select-columns
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
			value, err := readColumnValueFromBuffer(payload, columnOffsets[colIndex], serialTypes[colIndex])
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

	result["id"] = strconv.FormatInt(rowId, 10)

	return result, nil
}

func readVarintFromBuffer(buffer []byte, pos *int64) (int64, error) {
	var num int64

	for i := 0; i < MaxVarintBytes && *pos < int64(len(buffer)); i++ {
		currentByte := buffer[*pos]
		var value byte
		if i == 8 {
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

	return 0, fmt.Errorf("varint too long or buffer too short")
}

func readColumnValueFromBuffer(buffer []byte, offset int64, serialType int64) (string, error) {
	size := getContentSizeBySerialType(serialType)

	if offset+size > int64(len(buffer)) {
		return "", fmt.Errorf("buffer too short for column value")
	}

	valueBytes := buffer[offset : offset+size]

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

func getTableByName(name string, tables []Table) (*Table, error) {
	for _, table := range tables {
		if table.Name == name {
			return &table, nil
		}
	}
	return nil, fmt.Errorf("unable to find table: %s", name)
}

func parseSchemaEntries(ctx *DBContext, cellOffsets []int64) ([]Table, error) {
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
		values, err := readCell(ctx, offset, sqliteMasterColumns, schemaSelectInfo)
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

func handleSelectCount(ctx *DBContext, table *Table) {
	rowCount, err := CountTableRows(ctx, table)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(rowCount)
}

func handleSelectRows(ctx *DBContext, table *Table, selectInfo *SelectInfo) {
	rows, err := CollectTableRows(ctx, table, selectInfo)
	if err != nil {
		log.Fatal(err)
	}

	for _, row := range rows {
		fmt.Println(row)
	}
}

func handleSelect(ctx *DBContext, command string) {
	selectInfo, err := ParseSelect(command)
	if err != nil {
		log.Fatal(err)
	}
	selectInfo.WhereValue = strings.ReplaceAll(selectInfo.WhereValue, "'", "")

	table, err := getTableByName(selectInfo.TableName, ctx.Tables)
	if err != nil {
		log.Fatal(err)
	}

	if selectInfo.IsCount {
		handleSelectCount(ctx, table)
	} else {
		handleSelectRows(ctx, table, selectInfo)
	}
}

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
