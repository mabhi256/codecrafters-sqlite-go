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
	"time"
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

type PageType uint8

const (
	InteriorIndexPage PageType = 0x02
	InteriorTablePage PageType = 0x05
	LeafIndexPage     PageType = 0x0a
	LeafTablePage     PageType = 0x0d
)

type PageCache struct {
	pages map[int64]*Page
}

type DBContext struct {
	DBFile      *os.File
	PageSize    int64
	UsableSpace int64
	Tables      []Table
	Indexes     map[string]Index
	PageCache   *PageCache
	ReadBuffer  []byte
}

type Table struct {
	Name     string
	RootPage int64
	Columns  []Column
}

type Index struct {
	Name       string
	TableName  string
	ColumnName string
	IsUnique   bool
	RootPage   int64
}

type Column struct {
	Name         string
	Type         string
	IsPrimaryKey bool
}

type Page struct {
	PageNumber     int64
	PageOffset     int64
	PageType       PageType
	CellCount      int64
	CellOffsets    []int64
	Context        *DBContext
	RightmostChild int64
}

type TableCell struct {
	RowID  int64
	Values map[string]string
}

type IndexCell struct {
	Key    []byte
	RowIDs []int64
}

func NewPageCache() *PageCache {
	return &PageCache{
		pages: make(map[int64]*Page),
	}
}

func (pc *PageCache) Get(pageNumber int64) (*Page, bool) {
	page, exists := pc.pages[pageNumber]
	return page, exists
}

func (pc *PageCache) Put(pageNumber int64, page *Page) {
	pc.pages[pageNumber] = page
}

func (p *Page) IsLeaf() bool {
	return p.PageType == LeafTablePage || p.PageType == LeafIndexPage
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

func LoadPage(ctx *DBContext, pageNumber int64) (*Page, error) {
	// Check cache first
	if cachedPage, exists := ctx.PageCache.Get(pageNumber); exists {
		return cachedPage, nil
	}

	pageOffset := getPageOffset(pageNumber, ctx.PageSize)

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

	cellPointerArrayOffset := pageOffset + headerSize
	cellOffsets, err := getCellOffsets(ctx, cellPointerArrayOffset, cellCount)
	if err != nil {
		return nil, err
	}
	page.CellOffsets = cellOffsets

	// Cache the page
	ctx.PageCache.Put(pageNumber, page)

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

func (ctx *DBContext) readBytesAt(offset int64, size int64) ([]byte, error) {
	// Reuse buffer if possible
	if int64(cap(ctx.ReadBuffer)) < size {
		ctx.ReadBuffer = make([]byte, size*2) // Allocate extra space
	}
	buffer := ctx.ReadBuffer[:size]

	_, err := ctx.DBFile.Seek(offset, 0)
	if err != nil {
		return nil, fmt.Errorf("error seeking to offset %d: %v", offset, err)
	}

	n, err := ctx.DBFile.Read(buffer)
	if err != nil {
		return nil, fmt.Errorf("error reading %d bytes at offset %d: %v", size, offset, err)
	}
	if n != int(size) {
		return nil, fmt.Errorf("expected to read %d bytes, but read %d", size, n)
	}

	// Return a copy to avoid buffer reuse issues
	result := make([]byte, size)
	copy(result, buffer)
	return result, nil
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

func getUint16AtOffset(b []byte, offset int64) uint16 {
	return binary.BigEndian.Uint16(b[offset : offset+2])
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
		Indexes:     make(map[string]Index),
		PageCache:   NewPageCache(),
		ReadBuffer:  make([]byte, pageSize),
	}

	// Parse schema from page 1
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

func parseSchemaEntries(ctx *DBContext, cellOffsets []int64) ([]Table, error) {
	sqliteMasterColumns := []Column{
		{Name: "type", Type: "text"},
		{Name: "name", Type: "text"},
		{Name: "tbl_name", Type: "text"},
		{Name: "rootpage", Type: "integer"},
		{Name: "sql", Type: "text"},
	}

	schemaSelectInfo := &SelectInfo{
		Columns: []string{"type", "name", "tbl_name", "rootpage", "sql"},
	}

	var tables []Table
	for _, offset := range cellOffsets {
		values, err := readTableLeafCell(ctx, offset, sqliteMasterColumns, schemaSelectInfo)
		if err != nil {
			return nil, err
		}

		if len(values) == 0 { // filtered out
			continue
		}

		rootPageInt, _ := strconv.ParseInt(values["rootpage"], 10, 64)

		schemaType, exists := values["type"]
		if !exists {
			return nil, err
		}

		switch schemaType {
		case "table":
			table := Table{
				Name: values["tbl_name"],
			}

			table.RootPage = rootPageInt

			if table.Name != "sqlite_sequence" {
				table.ParseCreateTable(values["sql"])
			}

			tables = append(tables, table)
		case "index":
			index := Index{
				RootPage: rootPageInt,
			}
			index.ParseCreateIndex(values["sql"])

			ctx.Indexes[index.TableName] = index
		}

	}

	return tables, nil
}

func readCompletePayload(ctx *DBContext, payloadStartOffset int64, P int64, isTablePage bool) ([]byte, error) {
	U := ctx.UsableSpace

	// Calculate overflow parameters

	// Maximum payload without spilling onto an overflow page
	var X int64
	if isTablePage {
		X = U - 35
	} else {
		X = ((U - 12) * 64 / 255) - 23
	}
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

func deserializeRowID(serialType int64, data []byte) (int64, error) {
	value, err := serialTypeToValue(serialType, data)
	if err != nil {
		return 0, err
	}

	switch v := value.(type) {
	case int8:
		return int64(v), nil
	case int16:
		return int64(v), nil
	case int32:
		return int64(v), nil
	case int64:
		return v, nil
	default:
		return 0, fmt.Errorf("invalid rowID type: %T", v)
	}
}

func readTableLeafCell(ctx *DBContext, cellOffset int64, columns []Column, selectInfo *SelectInfo) (map[string]string, error) {
	cellHeaderBuffer, err := ctx.readBytesAt(cellOffset, 18)
	if err != nil {
		return nil, err
	}

	pos := int64(0)
	P, err := readVarintFromBuffer(cellHeaderBuffer, &pos)
	if err != nil {
		return nil, err
	}

	rowId, err := readVarintFromBuffer(cellHeaderBuffer, &pos)
	if err != nil {
		return nil, err
	}

	payloadStartOffset := cellOffset + pos
	completePayload, err := readCompletePayload(ctx, payloadStartOffset, P, true)
	if err != nil {
		return nil, err
	}

	return parseTableRecord(completePayload, rowId, columns, selectInfo)
}

func ReadTableLeafPage(page *Page, columns []Column, selectInfo *SelectInfo) ([]TableCell, error) {
	if page.PageType != LeafTablePage {
		return nil, fmt.Errorf("can only read table cells from table leaf pages")
	}

	var cells []TableCell
	for _, cellOffset := range page.CellOffsets {
		offset := page.PageOffset + cellOffset
		values, err := readTableLeafCell(page.Context, offset, columns, selectInfo)
		if err != nil {
			return nil, err
		}

		if len(values) == 0 {
			continue
		}

		rowIDStr, exists := values["id"]
		if !exists {
			continue
		}
		rowID, _ := strconv.ParseInt(rowIDStr, 10, 64)

		cells = append(cells, TableCell{
			RowID:  rowID,
			Values: values,
		})
	}

	return cells, nil
}

func parseTableRecord(payload []byte, rowId int64, columns []Column, selectInfo *SelectInfo) (map[string]string, error) {
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

func readIndexLeafCell(ctx *DBContext, cellOffset int64) (IndexCell, error) {
	cellHeaderBuffer, err := ctx.readBytesAt(cellOffset, 9)
	if err != nil {
		return IndexCell{}, err
	}

	pos := int64(0)
	payloadSize, err := readVarintFromBuffer(cellHeaderBuffer, &pos)
	if err != nil {
		return IndexCell{}, err
	}

	payloadStartOffset := cellOffset + pos
	payload, err := readCompletePayload(ctx, payloadStartOffset, payloadSize, false)
	if err != nil {
		return IndexCell{}, err
	}

	key, rowID, err := parseIndexRecord(payload)
	if err != nil {
		return IndexCell{}, err
	}

	return IndexCell{
		Key:    key,
		RowIDs: []int64{rowID},
	}, nil
}

func readIndexInteriorCellKey(ctx *DBContext, cellOffset int64) ([]byte, int64, error) {
	leftChildData, err := ctx.readBytesAt(cellOffset, 4)
	if err != nil {
		return nil, 0, err
	}
	leftChildPage := int64(binary.BigEndian.Uint32(leftChildData))

	payloadSizeBuffer, err := ctx.readBytesAt(cellOffset+4, 9)
	if err != nil {
		return nil, 0, err
	}

	pos := int64(0)
	payloadSize, err := readVarintFromBuffer(payloadSizeBuffer, &pos)
	if err != nil {
		return nil, 0, err
	}

	keyPayload, err := readCompletePayload(ctx, cellOffset+4+pos, payloadSize, false)
	if err != nil {
		return nil, 0, err
	}

	key, _, err := parseIndexRecord(keyPayload)
	if err != nil {
		return nil, 0, err
	}

	return key, leftChildPage, nil
}

func parseIndexRecord(payload []byte) ([]byte, int64, error) {
	pos := int64(0)

	recordHeaderSize, err := readVarintFromBuffer(payload, &pos)
	if err != nil {
		return nil, 0, err
	}

	keySerialType, err := readVarintFromBuffer(payload, &pos)
	if err != nil {
		return nil, 0, err
	}

	rowIDSerialType, err := readVarintFromBuffer(payload, &pos)
	if err != nil {
		return nil, 0, err
	}

	currentOffset := recordHeaderSize

	keySize := getContentSizeBySerialType(keySerialType)
	var keyData []byte
	if keySize > 0 {
		keyData = payload[currentOffset : currentOffset+keySize]
	}
	currentOffset += keySize

	rowIDSize := getContentSizeBySerialType(rowIDSerialType)
	rowIDData := payload[currentOffset : currentOffset+rowIDSize]
	rowID, err := deserializeRowID(rowIDSerialType, rowIDData)
	if err != nil {
		return nil, 0, err
	}

	return keyData, rowID, nil
}

func countRows(ctx *DBContext, pageNumber int64) (int64, error) {
	page, err := LoadPage(ctx, pageNumber)
	if err != nil {
		return 0, err
	}

	if page.IsLeaf() {
		return page.CellCount, nil
	}

	var totalRows int64

	for _, cellOffset := range page.CellOffsets {
		pos := page.PageOffset + cellOffset
		leftPointer, err := page.Context.readBytesAt(pos, 4)
		if err != nil {
			return 0, err
		}

		leftPageNumber := int64(binary.BigEndian.Uint32(leftPointer))
		childRows, err := countRows(ctx, leftPageNumber)
		if err != nil {
			return 0, err
		}
		totalRows += childRows
	}

	rightRows, err := countRows(ctx, page.RightmostChild)
	if err != nil {
		return 0, err
	}
	totalRows += rightRows

	return totalRows, nil
}

func (table *Table) findRowByID(ctx *DBContext, pageNumber int64, targetRowID int64, selectInfo *SelectInfo) (string, error) {
	page, err := LoadPage(ctx, pageNumber)
	if err != nil {
		return "", err
	}

	if page.IsLeaf() {
		// Binary search within the leaf page
		left, right := 0, int(page.CellCount)-1

		for left <= right {
			mid := (left + right) / 2
			cellOffset := page.PageOffset + page.CellOffsets[mid]

			// Read rowID from cell header
			cellHeaderBuffer, err := ctx.readBytesAt(cellOffset, 18)
			if err != nil {
				return "", err
			}

			pos := int64(0)
			// Skip payload size
			_, err = readVarintFromBuffer(cellHeaderBuffer, &pos)
			if err != nil {
				return "", err
			}
			// Read rowID
			rowID, err := readVarintFromBuffer(cellHeaderBuffer, &pos)
			if err != nil {
				return "", err
			}

			if rowID == targetRowID {
				// Found row; Read the full cell
				values, err := readTableLeafCell(ctx, cellOffset, table.Columns, selectInfo)
				if err != nil {
					return "", err
				}

				if len(values) == 0 { // Filtered out by WHERE clause
					return "", nil
				}

				resultValues := make([]string, len(selectInfo.Columns))
				for i, columnName := range selectInfo.Columns {
					resultValues[i] = values[columnName]
				}
				return strings.Join(resultValues, "|"), nil
			} else if rowID < targetRowID {
				left = mid + 1
			} else {
				right = mid - 1
			}
		}

		return "", nil // Not found
	}

	// Interior page - navigate to correct child using rowID comparisons
	for _, cellOffset := range page.CellOffsets {
		pos := page.PageOffset + cellOffset

		// Get separator rowID for this child
		separatorRowID, err := table.readTableInteriorCellRowID(ctx, pos)
		if err != nil {
			return "", err
		}

		// If targetRowID <= separatorRowID, go to left child
		if targetRowID <= separatorRowID {
			leftPointer, err := page.Context.readBytesAt(pos, 4)
			if err != nil {
				return "", err
			}
			leftPageNumber := int64(binary.BigEndian.Uint32(leftPointer))
			return table.findRowByID(ctx, leftPageNumber, targetRowID, selectInfo)
		}
	}

	// If targetRowID > all separator rowIDs, go to rightmost child
	return table.findRowByID(ctx, page.RightmostChild, targetRowID, selectInfo)
}

func (table *Table) readTableInteriorCellRowID(ctx *DBContext, cellOffset int64) (int64, error) {
	// Table interior cell structure: left_child_page (4 bytes) + rowid (varint)

	// Skip the 4-byte left child pointer
	rowIDBuffer, err := ctx.readBytesAt(cellOffset+4, 9) // max varint size
	if err != nil {
		return 0, err
	}

	pos := int64(0)
	rowID, err := readVarintFromBuffer(rowIDBuffer, &pos)
	if err != nil {
		return 0, err
	}

	return rowID, nil
}

func (table *Table) collectRowsFromPage(ctx *DBContext, pageNumber int64, selectInfo *SelectInfo, rows *[]string) error {
	page, err := LoadPage(ctx, pageNumber)
	if err != nil {
		return err
	}

	if page.IsLeaf() {
		cells, err := ReadTableLeafPage(page, table.Columns, selectInfo)
		if err != nil {
			return err
		}

		for _, cell := range cells {
			resultValues := make([]string, len(selectInfo.Columns))
			for i, columnName := range selectInfo.Columns {
				resultValues[i] = cell.Values[columnName]
			}
			*rows = append(*rows, strings.Join(resultValues, "|"))
		}
		return nil
	}

	for _, cellOffset := range page.CellOffsets {
		pos := page.PageOffset + cellOffset
		leftPointer, err := page.Context.readBytesAt(pos, 4)
		if err != nil {
			return err
		}

		leftPageNumber := int64(binary.BigEndian.Uint32(leftPointer))
		if err := table.collectRowsFromPage(ctx, leftPageNumber, selectInfo, rows); err != nil {
			return err
		}
	}

	return table.collectRowsFromPage(ctx, page.RightmostChild, selectInfo, rows)
}

func (index *Index) collectRowIDsFromIndex(ctx *DBContext, pageNumber int64, searchKey string, rowIDs *[]int64) error {
	page, err := LoadPage(ctx, pageNumber)
	if err != nil {
		return err
	}

	if page.IsLeaf() {
		err := index.searchLeafPage(ctx, page, searchKey, rowIDs)
		return err
	}

	// Interior page - traverse children in order
	for _, cellOffset := range page.CellOffsets {
		offset := page.PageOffset + cellOffset
		separatorKey, leftChildPage, err := readIndexInteriorCellKey(ctx, offset)
		if err != nil {
			return err
		}

		separatorKeyStr := string(separatorKey)

		// Process left child if it might contain our search key
		if strings.Compare(searchKey, separatorKeyStr) <= 0 {
			err = index.collectRowIDsFromIndex(ctx, leftChildPage, searchKey, rowIDs)
			if err != nil {
				return err
			}
		}

		// If searchKey < separatorKey, we don't need to check rightmost child
		if strings.Compare(searchKey, separatorKeyStr) < 0 {
			return nil
		}
	}

	// Process rightmost child
	err = index.collectRowIDsFromIndex(ctx, page.RightmostChild, searchKey, rowIDs)
	return err
}

func (index *Index) searchLeafPage(ctx *DBContext, page *Page, searchKey string, rowIDs *[]int64) error {
	// Binary search to find first potential match
	left, right := 0, int(page.CellCount)-1
	firstMatch := -1

	for left <= right {
		mid := (left + right) / 2
		offset := page.PageOffset + page.CellOffsets[mid]

		cell, err := readIndexLeafCell(ctx, offset)
		if err != nil {
			return err
		}

		keyStr := string(cell.Key)
		cmp := strings.Compare(keyStr, searchKey)

		if cmp == 0 {
			firstMatch = mid
			right = mid - 1 // Continue searching left for first occurrence
		} else if cmp < 0 {
			left = mid + 1
		} else {
			right = mid - 1
		}
	}

	if firstMatch == -1 {
		return nil // No matches in this page
	}

	// Collect all consecutive matches starting from firstMatch
	for i := firstMatch; i < int(page.CellCount); i++ {
		offset := page.PageOffset + page.CellOffsets[i]
		cell, err := readIndexLeafCell(ctx, offset)
		if err != nil {
			return err
		}

		keyStr := string(cell.Key)
		if keyStr != searchKey {
			break // No more matches (entries are sorted)
		}

		*rowIDs = append(*rowIDs, cell.RowIDs...)
	}

	return nil
}

func (table *Table) handleSelectCount(ctx *DBContext) {
	rowCount, err := countRows(ctx, table.RootPage)
	logErr(err)
	fmt.Println(rowCount)
}

func (table *Table) handleFullScan(ctx *DBContext, selectInfo *SelectInfo) {
	var rows []string
	err := table.collectRowsFromPage(ctx, table.RootPage, selectInfo, &rows)
	logErr(err)

	for _, row := range rows {
		fmt.Println(row)
	}
}

func (table *Table) handleIndexScan(ctx *DBContext, selectInfo *SelectInfo, index *Index) {
	startTime := time.Now()

	searchKey := selectInfo.WhereValue

	// Get all matching rowIDs from index
	var rowIDs []int64
	err := index.collectRowIDsFromIndex(ctx, index.RootPage, searchKey, &rowIDs)
	logErr(err)
	indexDuration := time.Since(startTime)

	lookupStartTime := time.Now()
	// Direct lookup for each rowID
	for _, rowID := range rowIDs {
		result, err := table.findRowByID(ctx, table.RootPage, rowID, selectInfo)
		logErr(err)

		fmt.Println(result)
	}
	lookupDuration := time.Since(lookupStartTime)
	totalDuration := time.Since(startTime)
	fmt.Fprintf(os.Stderr, "Index query completed: %d rows in %v (index: %v, lookups: %v)\n", len(rowIDs), totalDuration, indexDuration, lookupDuration)
}

func handleSelect(ctx *DBContext, command string) {
	selectInfo, err := ParseSelect(command)
	logErr(err)
	selectInfo.WhereValue = strings.ReplaceAll(selectInfo.WhereValue, "'", "")

	var table *Table
	for _, t := range ctx.Tables {
		if t.Name == selectInfo.TableName {
			table = &t
		}
	}

	index, indexExists := ctx.Indexes[selectInfo.TableName]

	if selectInfo.IsCount {
		table.handleSelectCount(ctx)
	} else if indexExists && selectInfo.WhereColumn == index.ColumnName {
		// fmt.Fprintf(os.Stderr, "Index scan test\n")
		table.handleIndexScan(ctx, selectInfo, &index)
	} else {
		table.handleFullScan(ctx, selectInfo)
	}
}

func logErr(err error) {
	if err != nil {
		log.Fatal(err)
		os.Exit(1)
	}
}

func main() {
	dbFilePath := os.Args[1]
	command := os.Args[2]

	ctx, err := initializeDB(dbFilePath)
	logErr(err)

	command = strings.TrimSpace(command)
	command = strings.ToLower(command)

	switch {
	case command == ".dbinfo":
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
