// Package main implements a MySQL Auto DB proxy that automatically creates databases
// when clients connect to them. This is designed for development and testing
// environments where you need to automatically provision databases for multiple services.
//
// The proxy intercepts MySQL connections, extracts the requested database name
// from the handshake, creates the database if it doesn't exist, and then
// forwards the connection to the real MySQL server.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/sirupsen/logrus"
)

// Config holds the proxy configuration
type Config struct {
	ProxyPort     int
	MySQLHost     string
	MySQLPort     int
	MySQLUser     string
	MySQLPassword string
	LogLevel      string
}

// Default configuration
var defaultConfig = Config{
	ProxyPort:     3308,
	MySQLHost:     "localhost",
	MySQLPort:     3306,
	MySQLUser:     "root",
	MySQLPassword: "test",
	LogLevel:      "info",
}

// setupLogging configures logrus based on the log level
func setupLogging(level string) {
	switch strings.ToLower(level) {
	case "debug":
		logrus.SetLevel(logrus.DebugLevel)
	case "info":
		logrus.SetLevel(logrus.InfoLevel)
	case "warn", "warning":
		logrus.SetLevel(logrus.WarnLevel)
	case "error":
		logrus.SetLevel(logrus.ErrorLevel)
	case "fatal":
		logrus.SetLevel(logrus.FatalLevel)
	case "panic":
		logrus.SetLevel(logrus.PanicLevel)
	default:
		logrus.SetLevel(logrus.InfoLevel)
	}

	// Set JSON formatter for structured logging
	logrus.SetFormatter(&logrus.JSONFormatter{
		TimestampFormat: time.RFC3339,
	})
}

// loadConfig loads configuration from environment variables or uses defaults
func loadConfig() Config {
	config := defaultConfig

	if port := os.Getenv("PROXY_PORT"); port != "" {
		if p, err := fmt.Sscanf(port, "%d", &config.ProxyPort); err != nil || p != 1 {
			logrus.Warnf("Invalid PROXY_PORT, using default: %d", config.ProxyPort)
		}
	}

	if host := os.Getenv("MYSQL_HOST"); host != "" {
		config.MySQLHost = host
	}

	if port := os.Getenv("MYSQL_PORT"); port != "" {
		if p, err := fmt.Sscanf(port, "%d", &config.MySQLPort); err != nil || p != 1 {
			logrus.Warnf("Invalid MYSQL_PORT, using default: %d", config.MySQLPort)
		}
	}

	if user := os.Getenv("MYSQL_USER"); user != "" {
		config.MySQLUser = user
	}

	if password := os.Getenv("MYSQL_PASSWORD"); password != "" {
		config.MySQLPassword = password
	}

	if level := os.Getenv("LOG_LEVEL"); level != "" {
		config.LogLevel = strings.ToLower(level)
	}

	return config
}

// MySQLPacket represents a MySQL protocol packet
type MySQLPacket struct {
	Length     int
	SequenceID int
	Payload    []byte
	FullPacket []byte
}

// readPacket reads a complete MySQL packet from the connection
func readPacket(conn net.Conn) (*MySQLPacket, error) {
	return readPacketWithTimeout(conn, 0)
}

// readPacketWithTimeout reads a complete MySQL packet from the connection with a timeout
func readPacketWithTimeout(conn net.Conn, timeout time.Duration) (*MySQLPacket, error) {
	if timeout > 0 {
		conn.SetReadDeadline(time.Now().Add(timeout))
		defer conn.SetReadDeadline(time.Time{}) // Clear deadline
	}

	// Read packet header (3 bytes length + 1 byte sequence ID)
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, fmt.Errorf("failed to read packet header: %w", err)
	}

	// Extract packet length (first 3 bytes, little-endian)
	packetLength := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
	sequenceID := int(header[3])

	// Read the packet payload
	payload := make([]byte, packetLength)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return nil, fmt.Errorf("failed to read packet payload: %w", err)
	}

	// Construct full packet
	fullPacket := append(header, payload...)

	return &MySQLPacket{
		Length:     packetLength,
		SequenceID: sequenceID,
		Payload:    payload,
		FullPacket: fullPacket,
	}, nil
}

// writePacket writes a MySQL packet to the connection
func writePacket(conn net.Conn, packet *MySQLPacket) error {
	_, err := conn.Write(packet.FullPacket)
	if err != nil {
		return fmt.Errorf("failed to write packet: %w", err)
	}
	return nil
}

// parseDatabaseName extracts the database name from a MySQL client handshake packet
func parseDatabaseName(packet *MySQLPacket) string {
	if len(packet.Payload) < 32 {
		logrus.WithField("payload_length", len(packet.Payload)).Debug("Packet too short for handshake")
		return ""
	}

	// Debug: Log packet structure
	logrus.WithFields(logrus.Fields{
		"payload_length": len(packet.Payload),
		"payload_hex":    fmt.Sprintf("%x", packet.Payload[:min(64, len(packet.Payload))]),
	}).Debug("Parsing handshake packet")

	// MySQL client handshake response structure:
	// - Capability flags (4 bytes)
	// - Max packet size (4 bytes)
	// - Character set (1 byte)
	// - Reserved (23 bytes)
	// - Username (null-terminated)
	// - Password (length-prefixed)
	// - Database name (null-terminated)

	pos := 32 // Skip capability flags, max packet size, character set, and reserved

	// Skip username (null-terminated)
	usernameStart := pos
	for pos < len(packet.Payload) && packet.Payload[pos] != 0 {
		pos++
	}
	username := string(packet.Payload[usernameStart:pos])
	pos++ // Skip null terminator

	logrus.WithField("username", username).Debug("Found username in handshake")

	if pos >= len(packet.Payload) {
		logrus.Debug("No password field found")
		return ""
	}

	// Skip password (length-prefixed)
	if pos < len(packet.Payload) {
		passwordLen := int(packet.Payload[pos])
		pos++              // Skip length byte
		pos += passwordLen // Skip password
		logrus.WithField("password_length", passwordLen).Debug("Skipped password")
	}

	if pos >= len(packet.Payload) {
		logrus.Debug("No database field found")
		return ""
	}

	// Check if there's more data after the password
	if pos >= len(packet.Payload) {
		logrus.Debug("No more data after password")
		return ""
	}

	// The next field could be either:
	// 1. Authentication method (null-terminated) - older clients
	// 2. Database name (null-terminated) - newer clients
	// 3. Client attributes - newer clients

	// Try to extract what comes after the password
	nextStart := pos
	nextEnd := nextStart
	for nextEnd < len(packet.Payload) && packet.Payload[nextEnd] != 0 {
		nextEnd++
	}

	if nextEnd > nextStart {
		nextField := string(packet.Payload[nextStart:nextEnd])
		logrus.WithField("next_field", nextField).Debug("Found field after password")

		// Check if this looks like an authentication method
		if nextField == "mysql_native_password" ||
			nextField == "caching_sha2_password" ||
			nextField == "sha256_password" {
			logrus.WithField("auth_method", nextField).Debug("Found authentication method")
			pos = nextEnd + 1 // Skip null terminator

			// Look for database name after auth method
			if pos >= len(packet.Payload) {
				logrus.Debug("No database field found after auth method")
				return ""
			}

			// Extract database name (null-terminated)
			dbStart := pos
			dbEnd := dbStart
			for dbEnd < len(packet.Payload) && packet.Payload[dbEnd] != 0 {
				dbEnd++
			}

			if dbEnd > dbStart {
				databaseName := string(packet.Payload[dbStart:dbEnd])
				// Check if this looks like a real database name (not client attributes)
				if len(databaseName) > 0 &&
					!strings.Contains(databaseName, "_client_") &&
					!strings.Contains(databaseName, "\x0c") {
					logrus.WithField("database", databaseName).Debug("Found database name in handshake")
					return databaseName
				} else {
					logrus.WithField("client_attributes", databaseName).Debug("Found client attributes, not database name")
				}
			}
		} else {
			// This might be the database name directly (no auth method specified)
			// Check if this looks like a real database name (not client attributes)
			if len(nextField) > 0 &&
				!strings.Contains(nextField, "_client_") &&
				!strings.Contains(nextField, "\x0c") {
				logrus.WithField("database", nextField).Debug("Found database name in handshake (no auth method)")
				return nextField
			} else {
				logrus.WithField("client_attributes", nextField).Debug("Found client attributes, not database name")
			}
		}
	}

	logrus.Debug("No database name found in handshake")
	return ""
}

// min returns the minimum of two integers
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// forwardWithUseInterception forwards data from client to MySQL while intercepting USE commands
func forwardWithUseInterception(clientConn, mysqlConn net.Conn, config Config, logger *logrus.Entry) {
	buffer := make([]byte, 4096)
	logger.Debug("Starting forwardWithUseInterception")
	for {
		n, err := clientConn.Read(buffer)
		if err != nil {
			if err != io.EOF {
				logger.WithError(err).Error("Error reading from client")
			} else {
				logger.Debug("Client closed connection (EOF)")
			}
			return
		}

		if n > 0 {
			logger.WithField("bytes_read", n).Debug("Read data from client")

			// Check if this is a USE command
			if isUseCommand(buffer[:n]) {
				databaseName := extractDatabaseFromUseCommand(buffer[:n])
				if databaseName != "" {
					logger.WithField("database", databaseName).Info("Intercepted USE command")
					if err := ensureDatabaseExists(config, databaseName); err != nil {
						logger.WithError(err).WithField("database", databaseName).Error("Failed to create database from USE command")
						// Continue anyway - let MySQL handle the error
					} else {
						logger.WithField("database", databaseName).Info("Database created from USE command")
					}
				}
			}

			// Forward the packet to MySQL
			_, err = mysqlConn.Write(buffer[:n])
			if err != nil {
				logger.WithError(err).Error("Error writing to MySQL")
				return
			}
			logger.WithField("bytes_written", n).Debug("Forwarded data to MySQL")
		}
	}
}

// isUseCommand checks if the packet contains a USE command
func isUseCommand(data []byte) bool {
	if len(data) < 5 {
		return false
	}

	// Skip packet header (4 bytes) and check for "USE " command
	commandStart := 4
	if commandStart+4 <= len(data) {
		command := string(data[commandStart : commandStart+4])
		return strings.ToUpper(command) == "USE "
	}
	return false
}

// extractDatabaseFromUseCommand extracts the database name from a USE command
func extractDatabaseFromUseCommand(data []byte) string {
	if len(data) < 8 {
		return ""
	}

	// Skip packet header (4 bytes) and "USE " (4 bytes)
	start := 8

	// Find the end of the database name (null terminator or end of packet)
	end := start
	for end < len(data) && data[end] != 0 && data[end] != ';' && data[end] != ' ' {
		end++
	}

	if end > start {
		return string(data[start:end])
	}
	return ""
}

// validateDatabaseName ensures the database name is safe to create
func validateDatabaseName(dbName string) error {
	if dbName == "" {
		return fmt.Errorf("database name cannot be empty")
	}

	// Check for SQL injection patterns
	if strings.Contains(strings.ToLower(dbName), "information_schema") ||
		strings.Contains(strings.ToLower(dbName), "mysql") ||
		strings.Contains(strings.ToLower(dbName), "performance_schema") ||
		strings.Contains(strings.ToLower(dbName), "sys") {
		return fmt.Errorf("database name '%s' is not allowed", dbName)
	}

	// Check for valid characters (alphanumeric, underscore, hyphen)
	validPattern := regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	if !validPattern.MatchString(dbName) {
		return fmt.Errorf("database name '%s' contains invalid characters", dbName)
	}

	return nil
}

// ensureDatabaseExists creates the database if it doesn't exist
func ensureDatabaseExists(config Config, dbName string) error {
	// Validate database name
	if err := validateDatabaseName(dbName); err != nil {
		return fmt.Errorf("invalid database name: %w", err)
	}

	// Create connection string
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/?timeout=10s&readTimeout=10s&writeTimeout=10s",
		config.MySQLUser, config.MySQLPassword, config.MySQLHost, config.MySQLPort)

	// Connect to MySQL
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return fmt.Errorf("failed to connect to MySQL: %w", err)
	}
	defer db.Close()

	// Set connection timeout
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Test the connection
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("failed to ping MySQL: %w", err)
	}

	// Check if database exists
	var exists int
	query := "SELECT COUNT(*) FROM INFORMATION_SCHEMA.SCHEMATA WHERE SCHEMA_NAME = ?"
	err = db.QueryRowContext(ctx, query, dbName).Scan(&exists)
	if err != nil {
		return fmt.Errorf("failed to check if database exists: %w", err)
	}

	if exists == 0 {
		// Database doesn't exist, create it
		createQuery := fmt.Sprintf("CREATE DATABASE `%s`", dbName)
		_, err = db.ExecContext(ctx, createQuery)
		if err != nil {
			return fmt.Errorf("failed to create database %s: %w", dbName, err)
		}
		logrus.WithField("database", dbName).Info("Created database")
	} else {
		logrus.WithField("database", dbName).Debug("Database already exists")
	}

	return nil
}

// handleConnection handles a single client connection
func handleConnection(config Config, clientConn net.Conn) {
	defer clientConn.Close()

	clientAddr := clientConn.RemoteAddr().String()
	logger := logrus.WithField("client_addr", clientAddr)
	logger.Info("New connection")

	// Connect to the real MySQL server
	mysqlAddr := net.JoinHostPort(config.MySQLHost, fmt.Sprintf("%d", config.MySQLPort))
	mysqlConn, err := net.DialTimeout("tcp", mysqlAddr, 10*time.Second)
	if err != nil {
		logger.WithError(err).WithField("mysql_addr", mysqlAddr).Error("Failed to connect to MySQL server")
		return
	}
	defer mysqlConn.Close()

	// Set timeouts on connections
	mysqlConn.SetDeadline(time.Now().Add(30 * time.Second))
	clientConn.SetDeadline(time.Now().Add(30 * time.Second))

	// Read the server greeting
	serverGreeting, err := readPacket(mysqlConn)
	if err != nil {
		logger.WithError(err).Error("Failed to read server greeting")
		return
	}

	// Send server greeting to client
	if err := writePacket(clientConn, serverGreeting); err != nil {
		logger.WithError(err).Error("Failed to send server greeting to client")
		return
	}

	// Read client handshake response
	clientHandshake, err := readPacket(clientConn)
	if err != nil {
		logger.WithError(err).Error("Failed to read client handshake")
		return
	}

	// Parse and handle database creation (but don't fail if parsing fails)
	databaseName := parseDatabaseName(clientHandshake)
	logger.WithField("database", databaseName).Debug("Parsed database name from handshake")

	// If database name found in handshake, create it immediately
	if databaseName != "" {
		logger.WithField("database", databaseName).Info("Client requested database in handshake")
		if err := ensureDatabaseExists(config, databaseName); err != nil {
			logger.WithError(err).WithField("database", databaseName).Error("Failed to create database")
			return
		}
		logger.WithField("database", databaseName).Info("Database is ready")
	} else {
		logger.Debug("No database specified in handshake - will handle USE commands later")
	}

	// Forward the client handshake to MySQL server
	if err := writePacket(mysqlConn, clientHandshake); err != nil {
		logger.WithError(err).Error("Failed to forward client handshake to MySQL")
		return
	}

	// Try to read MySQL server response, but be more lenient with timeouts
	serverResponse, err := readPacketWithTimeout(mysqlConn, 30*time.Second)
	if err != nil {
		logger.WithError(err).Warn("Failed to read MySQL server response - continuing anyway")
		// Send a simple OK packet to the client to keep it happy
		okPacket := &MySQLPacket{
			SequenceID: 2,
			Payload:    []byte{0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}, // OK packet
		}
		if err := writePacket(clientConn, okPacket); err != nil {
			logger.WithError(err).Error("Failed to send OK packet to client")
			return
		}
		logger.Debug("Sent OK packet to client")
	} else {
		// Forward server response to client
		if err := writePacket(clientConn, serverResponse); err != nil {
			logger.WithError(err).Error("Failed to forward server response to client")
			return
		}
		logger.WithFields(logrus.Fields{
			"response_length": len(serverResponse.Payload),
			"response_hex":    fmt.Sprintf("%x", serverResponse.Payload[:min(len(serverResponse.Payload), 20)]),
		}).Debug("Forwarded server response to client")
	}

	logger.Info("Handshake completed successfully")

	// Handle the rest of the connection by intercepting USE commands
	done := make(chan struct{})

	// Forward from client to MySQL with USE command interception
	go func() {
		defer close(done)
		forwardWithUseInterception(clientConn, mysqlConn, config, logger)
	}()

	// Forward from MySQL to client
	io.Copy(clientConn, mysqlConn)

	// Wait for the other goroutine to finish
	<-done
	logger.Info("Connection closed")
}

func main() {
	// Load configuration
	config := loadConfig()

	// Set up logging
	setupLogging(config.LogLevel)
	logrus.WithFields(logrus.Fields{
		"proxy_port": config.ProxyPort,
		"mysql_host": config.MySQLHost,
		"mysql_port": config.MySQLPort,
		"mysql_user": config.MySQLUser,
		"log_level":  config.LogLevel,
	}).Info("MySQL Auto DB Proxy starting")

	// Start the proxy server
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", config.ProxyPort))
	if err != nil {
		logrus.WithError(err).Fatal("Failed to start proxy server")
	}
	defer listener.Close()

	logrus.WithFields(logrus.Fields{
		"proxy_port": config.ProxyPort,
		"mysql_addr": net.JoinHostPort(config.MySQLHost, fmt.Sprintf("%d", config.MySQLPort)),
	}).Info("MySQL Auto DB Proxy started")

	// Accept and handle connections
	for {
		conn, err := listener.Accept()
		if err != nil {
			logrus.WithError(err).Error("Failed to accept connection")
			continue
		}

		go handleConnection(config, conn)
	}
}
