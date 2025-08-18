package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	// Available if you need it!
	// "github.com/xwb1989/sqlparser"
)

// Usage: your_program.sh sample.db .dbinfo
func main() {
	databaseFilePath := os.Args[1]
	command := os.Args[2]

	switch command {
	case ".dbinfo":
		databaseFile, err := os.Open(databaseFilePath)
		if err != nil {
			log.Fatal(err)
		}

		// You can use print statements as follows for debugging, they'll be visible when running tests.
		fmt.Fprintln(os.Stderr, "Logs from your program will appear here!")

		var pageSize uint16
		err = extractPageSize(databaseFile, &pageSize)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("database page size: %v\n", pageSize)

		var cellCount uint16
		extractCellCount(databaseFile, &cellCount)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("number of tables: %v\n", cellCount)

	default:
		fmt.Println("Unknown command", command)
		os.Exit(1)
	}
}

func getValueFromHeader(header []byte, offset, size int, value *uint16) error {
	valueRaw := bytes.NewReader(header[offset : offset+size])

	err := binary.Read(valueRaw, binary.BigEndian, value)
	if err != nil {
		return fmt.Errorf("unable to read uint16")
	}

	return nil
}

func extractPageSize(dbFile *os.File, pageSize *uint16) error {
	header := make([]byte, 100)

	_, err := dbFile.Read(header)
	if err != nil {
		log.Fatal(err)
	}

	if err := getValueFromHeader(header, 16, 2, pageSize); err != nil {
		return fmt.Errorf("failed to page size: %w", err)
	}

	return nil
}

func extractCellCount(dbFile *os.File, cellCount *uint16) error {
	header := make([]byte, 8)

	_, err := dbFile.ReadAt(header, 100)
	if err != nil {
		log.Fatal(err)
	}

	if err := getValueFromHeader(header, 3, 2, cellCount); err != nil {
		return fmt.Errorf("failed to read cell count: %w", err)
	}

	return nil
}
