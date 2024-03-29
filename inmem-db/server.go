// server.go
package main

import (
	"bufio"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Database struct {
	data      map[string]string
	expiry    map[string]time.Time
	sortedSet map[string]map[string]float64
	mu        sync.Mutex
}

func NewDatabase() *Database {
	return &Database{
		data:      make(map[string]string),
		expiry:    make(map[string]time.Time),
		sortedSet: make(map[string]map[string]float64),
	}
}

func (db *Database) handleCommand(command string) string {
	parts := strings.Fields(command)
	if len(parts) == 0 {
		return errorResponse("Empty Command")
	}

	switch strings.ToUpper(parts[0]) {
	case "GET":
		return db.get(parts)
	case "SET":
		return db.set(splitCommand(command))
	case "DEL":
		return db.del(parts)
	case "EXPIRE":
		return db.expire(parts)
	case "KEYS":
		if len(parts) != 2 {
			return errorResponse("wrong number of arguments for 'KEYS' command")
		}
		keys := db.keys(parts[1])
		if keys == "-1\r\n" {
			return keys // No key matches
		}
		return keys // Add single \r\n after entire response is constructed

	case "TTL":
		return db.ttl(parts)
	case "ZADD":
		return db.zadd(parts)
	case "ZRANGE":
		return db.zrange(parts)
	default:
		return fmt.Sprintf("-ERR Unknown command '%s'\r\n", parts[0])
	}
}

func splitCommand(cmd string) []string {
	var parts []string
	inQuotes := false
	currentPart := ""
	for _, char := range cmd {
		if char == '"' {
			inQuotes = !inQuotes
		} else if char == ' ' && !inQuotes {
			if currentPart != "" {
				parts = append(parts, currentPart)
				currentPart = ""
			}
			continue
		}
		currentPart += string(char)
	}
	if currentPart != "" {
		parts = append(parts, currentPart)
	}
	return parts
}

func (db *Database) get(parts []string) string {
	if len(parts) != 2 {
		return errorResponse("wrong number of arguments for 'GET' command")
	}
	value, ok := db.data[parts[1]]
	if !ok {
		return "$-1\r\n" // Key not found
	}
	key := parts[1]
	expireTime, ok := db.expiry[key]
	if ok && expireTime.Before(time.Now()) {
		delete(db.data, key)   // Remove the expired key
		delete(db.expiry, key) // Remove the expiration time entry
		return "$-1\r\n"       // Key has expired
	}
	return fmt.Sprintf("$%s\r\n", value)
}

func (db *Database) set(parts []string) string {
	if len(parts) != 3 && len(parts) != 5 {
		return errorResponse("wrong number of arguments for 'SET' command")
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	key := parts[1]
	value := parts[2]
	db.data[key] = value
	if len(parts) >= 5 && strings.ToUpper(parts[3]) == "EX" {
		expireTime, err := strconv.Atoi(parts[4])
		if err != nil {
			return errorResponse("Invalid expiration time")
		}
		db.expiry[key] = time.Now().Add(time.Second * time.Duration(expireTime))
		// Set expiration time using a goroutine
		go func(key string, expireTime int) {
			<-time.After(time.Duration(expireTime) * time.Second)
			delete(db.data, key)
		}(key, expireTime)
	}
	return "+OK\r\n"
}

func (db *Database) del(parts []string) string {
	if len(parts) < 2 {
		return errorResponse("Wrong number of arguments for 'DEL' command")
	}
	count := 0
	for i := 1; i < len(parts); i++ {
		if _, ok := db.data[parts[i]]; ok {
			delete(db.data, parts[i])
			count++
		}
	}
	return fmt.Sprintf(":%d\r\n", count)
}

func (db *Database) expire(parts []string) string {
	if len(parts) != 3 {
		return errorResponse("wrong number of arguments for 'EXPIRE' command")
	}
	key := parts[1]
	expiry, err := strconv.Atoi(parts[2])
	if err != nil {
		return "-ERR invalid expire time\r\n"
	}
	db.expiry[key] = time.Now().Add(time.Second * time.Duration(expiry))
	return "$:1\r\n"
}

func match(pattern, key string) bool {
	i, j := 0, 0
	for i < len(pattern) && j < len(key) {
		if pattern[i] == '?' || pattern[i] == key[j] {
			i++
			j++
		} else if pattern[i] == '*' {
			if i+1 < len(pattern) && pattern[i+1] == key[j] {
				i++
			} else {
				j++
			}
		} else {
			return false
		}
	}
	return i == len(pattern) && j == len(key)
}

func (db *Database) keys(pattern string) string {
	if pattern == "*" {
		var response strings.Builder
		for key := range db.data {
			response.WriteString(fmt.Sprintf("\"%s\"\r\n", key))
		}
		response.WriteString("-1\r\n") // Indicate end of response
		return response.String()
	}

	var result []string
	for key := range db.data {
		if match(pattern, key) {
			result = append(result, key)
		}
	}
	if len(result) == 0 {
		return "-1\r\n" // Return -1\r\n if there are no key matches
	}
	var response strings.Builder
	for _, key := range result {
		response.WriteString(fmt.Sprintf("\"%s\"\r\n", key))
	}
	response.WriteString("-1\r\n") // Indicate end of response
	return response.String()
}

func (db *Database) ttl(parts []string) string {
	if len(parts) != 2 {
		return errorResponse("wrong number of arguments for 'TTL' command")
	}
	key := parts[1]
	if expiry, ok := db.expiry[key]; ok {
		ttl := expiry.Sub(time.Now())
		if ttl > 0 {
			return fmt.Sprintf(":%d\r\n", int(ttl.Seconds()))
		}
	}
	return ":-1\r\n"
}

func (db *Database) zadd(parts []string) string {
	if len(parts) < 4 || (len(parts)-2)%2 != 0 {
		return errorResponse(" wrong number of arguments for 'ZADD' command")
	}
	db.mu.Lock()
	defer db.mu.Unlock()

	key := parts[1]
	set, ok := db.sortedSet[key]
	if !ok {
		set = make(map[string]float64)
		db.sortedSet[key] = set
	}

	count := 0
	for i := 2; i < len(parts); i += 2 {
		score, err := strconv.ParseFloat(parts[i], 64)
		if err != nil {
			return "-ERR invalid score\r\n"
		}
		member := parts[i+1]
		set[member] = score
		count++
	}

	return fmt.Sprintf(":%d\r\n", count)
}

func (db *Database) zrange(parts []string) string {
	if len(parts) < 4 {
		return errorResponse(" wrong number of arguments for 'ZRANGE' command")
	}

	key := parts[1]
	set, ok := db.sortedSet[key]
	if !ok {
		return "-1\r\n" // Key not found
	}

	start, err := strconv.Atoi(parts[2])
	if err != nil {
		return "-ERR invalid start index\r\n"
	}
	end, err := strconv.Atoi(parts[3])
	if err != nil {
		return "-ERR invalid end index\r\n"
	}

	if start < 0 {
		start = len(set) + start
	}
	if end < 0 {
		end = len(set) + end
	}

	if start > end || start >= len(set) {
		return "-1\r\n" // No elements in range
	}

	var response strings.Builder
	count := 0
	for member, score := range set {
		if count >= start && count <= end {
			response.WriteString(fmt.Sprintf("%s\r\n", member))
			response.WriteString(fmt.Sprintf("%.0f\r\n", score))
		}
		count++
	}
	return response.String()
}

func (db *Database) expired(key string) bool {
	expiry, ok := db.expiry[key]
	if !ok {
		return false
	}
	return time.Now().After(expiry)
}

func handleConnection(conn net.Conn, db *Database) {
	defer conn.Close()

	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)

	for {
		cmd, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		cmd = strings.TrimSpace(cmd)
		if cmd == "QUIT" {
			return
		}

		response := db.handleCommand(cmd)
		writer.WriteString(response)
		writer.Flush()
	}
}

func errorResponse(message string) string {
	return "-ERR " + message + "\r\n"
}

func main() {
	db := NewDatabase()

	listener, err := net.Listen("tcp", ":6379")
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	defer listener.Close()

	for {
		conn, err := listener.Accept()
		if err != nil {
			fmt.Println("Error accepting connection:", err)
			continue
		}
		go handleConnection(conn, db)
	}
}
