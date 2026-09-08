package api

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/redisqueue"
	log "github.com/sirupsen/logrus"
)

const (
	redisUsageChannel  = "usage"
	redisErrorsChannel = "errors"
)

type redisSubscriptionCommand struct {
	args []string
	err  error
}

func isRedisRESPPrefix(prefix byte) bool {
	switch prefix {
	case '*', '$', '+', '-', ':':
		return true
	default:
		return false
	}
}

func (s *Server) handleRedisConnection(conn net.Conn, reader *bufio.Reader) {
	if s == nil || conn == nil {
		return
	}
	if reader == nil {
		reader = bufio.NewReader(conn)
	}

	clientIP, localClient := resolveRemoteIP(conn.RemoteAddr())
	authed := false
	writer := bufio.NewWriter(conn)
	defer func() {
		if errClose := conn.Close(); errClose != nil {
			log.Errorf("redis connection close error: %v", errClose)
		}
	}()

	flush := func() bool {
		if errFlush := writer.Flush(); errFlush != nil {
			log.Errorf("redis protocol flush error: %v", errFlush)
			return false
		}
		return true
	}

	if s.cfg != nil && s.cfg.Home.Enabled {
		_ = writeRedisError(writer, "ERR redis usage output disabled in home mode")
		_ = writer.Flush()
		return
	}

	for {
		if !s.managementRoutesEnabled.Load() {
			return
		}

		args, errRead := readRESPArray(reader)
		if errRead != nil {
			if !errors.Is(errRead, io.EOF) {
				_ = writeRedisError(writer, "ERR "+errRead.Error())
				_ = writer.Flush()
			}
			return
		}
		if len(args) == 0 {
			_ = writeRedisError(writer, "ERR empty command")
			if !flush() {
				return
			}
			continue
		}

		cmd := strings.ToUpper(strings.TrimSpace(args[0]))

		if cmd != "AUTH" && !authed {
			if s.mgmt != nil {
				_, statusCode, errMsg := s.mgmt.AuthenticateManagementKey(clientIP, localClient, "")
				if statusCode == http.StatusForbidden && strings.HasPrefix(errMsg, "IP banned due to too many failed attempts") {
					_ = writeRedisError(writer, "ERR "+errMsg)
				} else {
					_ = writeRedisError(writer, "NOAUTH Authentication required.")
				}
			} else {
				_ = writeRedisError(writer, "NOAUTH Authentication required.")
			}
			if !flush() {
				return
			}
			continue
		}

		switch cmd {
		case "AUTH":
			password, ok := parseAuthPassword(args)
			if !ok {
				if s.mgmt != nil {
					_, statusCode, errMsg := s.mgmt.AuthenticateManagementKey(clientIP, localClient, "")
					if statusCode == http.StatusForbidden && strings.HasPrefix(errMsg, "IP banned due to too many failed attempts") {
						_ = writeRedisError(writer, "ERR "+errMsg)
						if !flush() {
							return
						}
						continue
					}
				}
				_ = writeRedisError(writer, "ERR wrong number of arguments for 'auth' command")
				if !flush() {
					return
				}
				continue
			}
			if s.mgmt == nil {
				_ = writeRedisError(writer, "ERR remote management disabled")
				if !flush() {
					return
				}
				continue
			}
			allowed, _, errMsg := s.mgmt.AuthenticateManagementKey(clientIP, localClient, password)
			if !allowed {
				_ = writeRedisError(writer, "ERR "+errMsg)
				if !flush() {
					return
				}
				continue
			}
			authed = true
			_ = writeRedisSimpleString(writer, "OK")
			if !flush() {
				return
			}
		case "SUBSCRIBE":
			channel, ok := parseSubscribeChannel(args)
			if !ok {
				_ = writeRedisError(writer, "ERR wrong number of arguments for 'subscribe' command")
				if !flush() {
					return
				}
				continue
			}
			messages, unsubscribe, ok := subscribeRedisChannel(channel)
			if !ok {
				_ = writeRedisError(writer, fmt.Sprintf("ERR unsupported channel '%s'", channel))
				if !flush() {
					return
				}
				continue
			}
			if errWrite := writeRedisPubSubSubscribe(writer, channel, 1); errWrite != nil {
				unsubscribe()
				log.Errorf("redis protocol subscribe response error: %v", errWrite)
				return
			}
			if !flush() {
				unsubscribe()
				return
			}
			s.streamRedisSubscription(reader, writer, channel, messages, unsubscribe)
			return
		case "LPOP", "RPOP":
			count, hasCount, ok := parsePopCount(args)
			if !ok {
				_ = writeRedisError(writer, "ERR wrong number of arguments for '"+strings.ToLower(cmd)+"' command")
				if !flush() {
					return
				}
				continue
			}
			if count <= 0 {
				_ = writeRedisError(writer, "ERR value is not an integer or out of range")
				if !flush() {
					return
				}
				continue
			}
			items, ok := popRedisQueueItems(args[1], count)
			if !ok {
				_ = writeRedisError(writer, fmt.Sprintf("ERR unsupported channel '%s'", strings.TrimSpace(args[1])))
				if !flush() {
					return
				}
				continue
			}
			if hasCount {
				_ = writeRedisArrayOfBulkStrings(writer, items)
				if !flush() {
					return
				}
				continue
			}
			if len(items) == 0 {
				_ = writeRedisNilBulkString(writer)
				if !flush() {
					return
				}
				continue
			}
			_ = writeRedisBulkString(writer, items[0])
			if !flush() {
				return
			}
		case "XADD":
			entry, errMsg := handleXAdd(args)
			if errMsg != "" {
				_ = writeRedisError(writer, "ERR "+errMsg)
				if !flush() {
					return
				}
				continue
			}
			_ = writeRedisBulkString(writer, []byte(entry))
			if !flush() {
				return
			}
		case "XLEN":
			if len(args) != 2 {
				_ = writeRedisError(writer, "ERR wrong number of arguments for 'xlen' command")
				if !flush() {
					return
				}
				continue
			}
			_ = writeRedisInteger(writer, redisqueue.Stream(args[1]).Len())
			if !flush() {
				return
			}
		case "XGROUP":
			if errMsg := handleXGroup(args); errMsg != "" {
				_ = writeRedisError(writer, "ERR "+errMsg)
				if !flush() {
					return
				}
				continue
			}
			_ = writeRedisSimpleString(writer, "OK")
			if !flush() {
				return
			}
		case "XREADGROUP":
			key, entries, errMsg := handleXReadGroup(args)
			if errMsg != "" {
				_ = writeRedisError(writer, "ERR "+errMsg)
				if !flush() {
					return
				}
				continue
			}
			if errWrite := writeStreamReadResult(writer, key, entries); errWrite != nil {
				log.Errorf("redis protocol stream read write error: %v", errWrite)
				return
			}
			if !flush() {
				return
			}
		case "XACK":
			count, errMsg := handleXAck(args)
			if errMsg != "" {
				_ = writeRedisError(writer, "ERR "+errMsg)
				if !flush() {
					return
				}
				continue
			}
			_ = writeRedisInteger(writer, count)
			if !flush() {
				return
			}
		case "PUBLISH":
			if len(args) < 3 {
				_ = writeRedisError(writer, "ERR wrong number of arguments for 'publish' command")
				if !flush() {
					return
				}
				continue
			}
			if strings.EqualFold(strings.TrimSpace(args[1]), redisqueue.BlacklistChannel) {
				redisqueue.PublishBlacklist([]byte(args[2]))
				_ = writeRedisInteger(writer, redisqueue.BlacklistSubscriberCount())
				if !flush() {
					return
				}
				continue
			}
			_ = writeRedisError(writer, fmt.Sprintf("ERR unsupported channel '%s'", strings.TrimSpace(args[1])))
			if !flush() {
				return
			}
		case "BLACKLIST":
			// BLACKLIST <auth_index> — returns 1 if the index is currently blocked,
			// 0 otherwise. Used for introspection and verification of the local gate.
			if len(args) != 2 {
				_ = writeRedisError(writer, "ERR wrong number of arguments for 'blacklist' command")
				if !flush() {
					return
				}
				continue
			}
			if redisqueue.IsKeyBlocked(args[1]) {
				_ = writeRedisInteger(writer, 1)
			} else {
				_ = writeRedisInteger(writer, 0)
			}
			if !flush() {
				return
			}
		default:
			_ = writeRedisError(writer, fmt.Sprintf("ERR unknown command '%s'", strings.ToLower(cmd)))
			if !flush() {
				return
			}
		}
	}
}

func subscribeRedisChannel(channel string) (<-chan []byte, func(), bool) {
	switch strings.ToLower(strings.TrimSpace(channel)) {
	case redisUsageChannel:
		messages, unsubscribe := redisqueue.SubscribeUsage()
		return messages, unsubscribe, true
	case redisErrorsChannel:
		messages, unsubscribe := redisqueue.SubscribeErrors()
		return messages, unsubscribe, true
	case redisqueue.BlacklistChannel:
		messages, unsubscribe := redisqueue.SubscribeBlacklist()
		return messages, unsubscribe, true
	default:
		return nil, nil, false
	}
}

func popRedisQueueItems(channel string, count int) ([][]byte, bool) {
	switch strings.ToLower(strings.TrimSpace(channel)) {
	case redisUsageChannel:
		return redisqueue.PopOldest(count), true
	default:
		return nil, false
	}
}

func (s *Server) streamRedisSubscription(reader *bufio.Reader, writer *bufio.Writer, channel string, messages <-chan []byte, unsubscribe func()) {
	if unsubscribe == nil {
		return
	}
	defer unsubscribe()

	done := make(chan struct{})
	defer close(done)

	commands := make(chan redisSubscriptionCommand, 1)
	go readRedisSubscriptionCommands(reader, commands, done)

	for {
		select {
		case msg, ok := <-messages:
			if !ok {
				return
			}
			if errWrite := writeRedisPubSubMessage(writer, channel, msg); errWrite != nil {
				log.Errorf("redis protocol publish message error: %v", errWrite)
				return
			}
			if errFlush := writer.Flush(); errFlush != nil {
				log.Errorf("redis protocol flush error: %v", errFlush)
				return
			}
		case command, ok := <-commands:
			if !ok {
				return
			}
			keepOpen := handleRedisSubscriptionCommand(writer, channel, command)
			if errFlush := writer.Flush(); errFlush != nil {
				log.Errorf("redis protocol flush error: %v", errFlush)
				return
			}
			if !keepOpen {
				return
			}
		}
	}
}

func readRedisSubscriptionCommands(reader *bufio.Reader, commands chan<- redisSubscriptionCommand, done <-chan struct{}) {
	defer close(commands)

	for {
		args, errRead := readRESPArray(reader)
		if errRead != nil {
			if !errors.Is(errRead, io.EOF) {
				select {
				case commands <- redisSubscriptionCommand{err: errRead}:
				case <-done:
				}
			}
			return
		}
		select {
		case commands <- redisSubscriptionCommand{args: args}:
		case <-done:
			return
		}
	}
}

func handleRedisSubscriptionCommand(writer *bufio.Writer, channel string, command redisSubscriptionCommand) bool {
	if command.err != nil {
		_ = writeRedisError(writer, "ERR "+command.err.Error())
		return false
	}
	if len(command.args) == 0 {
		_ = writeRedisError(writer, "ERR empty command")
		return true
	}

	cmd := strings.ToUpper(strings.TrimSpace(command.args[0]))
	switch cmd {
	case "PING":
		payload := []byte(nil)
		if len(command.args) > 1 {
			payload = []byte(command.args[1])
		}
		_ = writeRedisPubSubPong(writer, payload)
		return true
	case "UNSUBSCRIBE":
		_ = writeRedisPubSubUnsubscribe(writer, channel, 0)
		return false
	case "QUIT":
		_ = writeRedisSimpleString(writer, "OK")
		return false
	default:
		_ = writeRedisError(writer, fmt.Sprintf("ERR unknown command '%s'", strings.ToLower(cmd)))
		return true
	}
}

func resolveRemoteIP(addr net.Addr) (ip string, localClient bool) {
	if addr == nil {
		return "", false
	}

	var host string
	switch a := addr.(type) {
	case *net.TCPAddr:
		if a != nil && a.IP != nil {
			if ip4 := a.IP.To4(); ip4 != nil {
				host = ip4.String()
			} else {
				host = a.IP.String()
			}
		}
	default:
		host = addr.String()
		if h, _, errSplit := net.SplitHostPort(host); errSplit == nil {
			host = h
		}
		host = strings.TrimSpace(host)
		if raw, _, ok := strings.Cut(host, "%"); ok {
			host = raw
		}
		if parsed := net.ParseIP(host); parsed != nil {
			if ip4 := parsed.To4(); ip4 != nil {
				host = ip4.String()
			} else {
				host = parsed.String()
			}
		}
	}

	host = strings.TrimSpace(host)
	localClient = host == "127.0.0.1" || host == "::1"
	return host, localClient
}

func parseAuthPassword(args []string) (string, bool) {
	switch len(args) {
	case 2:
		return args[1], true
	case 3:
		return args[2], true
	default:
		return "", false
	}
}

func parseSubscribeChannel(args []string) (string, bool) {
	if len(args) != 2 {
		return "", false
	}
	return strings.TrimSpace(args[1]), true
}

func parsePopCount(args []string) (count int, hasCount bool, ok bool) {
	if len(args) != 2 && len(args) != 3 {
		return 0, false, false
	}
	if len(args) == 2 {
		return 1, false, true
	}
	parsed, errParse := strconv.Atoi(strings.TrimSpace(args[2]))
	if errParse != nil {
		return 0, true, true
	}
	return parsed, true, true
}

func readRESPArray(reader *bufio.Reader) ([]string, error) {
	prefix, errRead := reader.ReadByte()
	if errRead != nil {
		return nil, errRead
	}
	if prefix != '*' {
		return nil, fmt.Errorf("protocol error")
	}
	line, errLine := readRESPLine(reader)
	if errLine != nil {
		return nil, errLine
	}
	count, errParse := strconv.Atoi(line)
	if errParse != nil || count < 0 {
		return nil, fmt.Errorf("protocol error")
	}
	args := make([]string, 0, count)
	for i := 0; i < count; i++ {
		value, errString := readRESPString(reader)
		if errString != nil {
			return nil, errString
		}
		args = append(args, value)
	}
	return args, nil
}

func readRESPString(reader *bufio.Reader) (string, error) {
	prefix, errRead := reader.ReadByte()
	if errRead != nil {
		return "", errRead
	}
	switch prefix {
	case '$':
		return readRESPBulkString(reader)
	case '+', ':':
		return readRESPLine(reader)
	default:
		return "", fmt.Errorf("protocol error")
	}
}

func readRESPBulkString(reader *bufio.Reader) (string, error) {
	line, errLine := readRESPLine(reader)
	if errLine != nil {
		return "", errLine
	}
	length, errParse := strconv.Atoi(line)
	if errParse != nil {
		return "", fmt.Errorf("protocol error")
	}
	if length < 0 {
		return "", nil
	}
	buf := make([]byte, length+2)
	if _, errRead := io.ReadFull(reader, buf); errRead != nil {
		return "", errRead
	}
	if length+2 < 2 || buf[length] != '\r' || buf[length+1] != '\n' {
		return "", fmt.Errorf("protocol error")
	}
	return string(buf[:length]), nil
}

func readRESPLine(reader *bufio.Reader) (string, error) {
	line, errRead := reader.ReadString('\n')
	if errRead != nil {
		return "", errRead
	}
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	return line, nil
}

func writeRedisSimpleString(writer *bufio.Writer, value string) error {
	if writer == nil {
		return net.ErrClosed
	}
	_, errWrite := writer.WriteString("+" + value + "\r\n")
	return errWrite
}

func writeRedisError(writer *bufio.Writer, message string) error {
	if writer == nil {
		return net.ErrClosed
	}
	_, errWrite := writer.WriteString("-" + message + "\r\n")
	return errWrite
}

func writeRedisNilBulkString(writer *bufio.Writer) error {
	if writer == nil {
		return net.ErrClosed
	}
	_, errWrite := writer.WriteString("$-1\r\n")
	return errWrite
}

func writeRedisBulkString(writer *bufio.Writer, payload []byte) error {
	if writer == nil {
		return net.ErrClosed
	}
	if payload == nil {
		return writeRedisNilBulkString(writer)
	}
	if _, errWrite := writer.WriteString("$" + strconv.Itoa(len(payload)) + "\r\n"); errWrite != nil {
		return errWrite
	}
	if _, errWrite := writer.Write(payload); errWrite != nil {
		return errWrite
	}
	_, errWrite := writer.WriteString("\r\n")
	return errWrite
}

func writeRedisArrayOfBulkStrings(writer *bufio.Writer, items [][]byte) error {
	if writer == nil {
		return net.ErrClosed
	}
	if _, errWrite := writer.WriteString("*" + strconv.Itoa(len(items)) + "\r\n"); errWrite != nil {
		return errWrite
	}
	for i := range items {
		if errWrite := writeRedisBulkString(writer, items[i]); errWrite != nil {
			return errWrite
		}
	}
	return nil
}

func writeRedisInteger(writer *bufio.Writer, value int) error {
	if writer == nil {
		return net.ErrClosed
	}
	_, errWrite := writer.WriteString(":" + strconv.Itoa(value) + "\r\n")
	return errWrite
}

func writeRedisArrayHeader(writer *bufio.Writer, count int) error {
	if writer == nil {
		return net.ErrClosed
	}
	_, errWrite := writer.WriteString("*" + strconv.Itoa(count) + "\r\n")
	return errWrite
}

func writeRedisPubSubSubscribe(writer *bufio.Writer, channel string, count int) error {
	if errWrite := writeRedisArrayHeader(writer, 3); errWrite != nil {
		return errWrite
	}
	if errWrite := writeRedisBulkString(writer, []byte("subscribe")); errWrite != nil {
		return errWrite
	}
	if errWrite := writeRedisBulkString(writer, []byte(channel)); errWrite != nil {
		return errWrite
	}
	return writeRedisInteger(writer, count)
}

func writeRedisPubSubUnsubscribe(writer *bufio.Writer, channel string, count int) error {
	if errWrite := writeRedisArrayHeader(writer, 3); errWrite != nil {
		return errWrite
	}
	if errWrite := writeRedisBulkString(writer, []byte("unsubscribe")); errWrite != nil {
		return errWrite
	}
	if errWrite := writeRedisBulkString(writer, []byte(channel)); errWrite != nil {
		return errWrite
	}
	return writeRedisInteger(writer, count)
}

func writeRedisPubSubMessage(writer *bufio.Writer, channel string, payload []byte) error {
	if errWrite := writeRedisArrayHeader(writer, 3); errWrite != nil {
		return errWrite
	}
	if errWrite := writeRedisBulkString(writer, []byte("message")); errWrite != nil {
		return errWrite
	}
	if errWrite := writeRedisBulkString(writer, []byte(channel)); errWrite != nil {
		return errWrite
	}
	return writeRedisBulkString(writer, payload)
}

func writeRedisPubSubPong(writer *bufio.Writer, payload []byte) error {
	if errWrite := writeRedisArrayHeader(writer, 2); errWrite != nil {
		return errWrite
	}
	if errWrite := writeRedisBulkString(writer, []byte("pong")); errWrite != nil {
		return errWrite
	}
	return writeRedisBulkString(writer, payload)
}

// handleXAdd implements `XADD key [NOMKSTREAM] [MAXLEN [~|=] n] * field value...`.
// It returns the generated entry ID on success, or an error message.
func handleXAdd(args []string) (string, string) {
	if len(args) < 4 {
		return "", "wrong number of arguments for 'xadd' command"
	}
	key := args[1]
	// Parse the ID token: accept "*" (auto) or an explicit numeric ID. Tokens
	// NOMKSTREAM / MAXLEN are consumed and ignored (bounded memory is handled by
	// the proxy's retention pruning).
	idIdx := 2
	if len(args) > 3 {
		switch strings.ToUpper(args[2]) {
		case "NOMKSTREAM":
			idIdx = 3
		case "MAXLEN":
			// MAXLEN [~|=] count
			if len(args) >= 5 {
				idIdx = 4
			} else {
				idIdx = 3
			}
		}
	}
	if idIdx >= len(args) {
		return "", "wrong number of arguments for 'xadd' command"
	}
	idToken := args[idIdx]
	fields := make(map[string]string)
	fieldIdx := idIdx + 1
	if (len(args)-fieldIdx)%2 != 0 || fieldIdx >= len(args) {
		return "", "wrong number of arguments for 'xadd' command"
	}
	for i := fieldIdx; i < len(args); i += 2 {
		fields[args[i]] = args[i+1]
	}

	st := redisqueue.Stream(key)
	if idToken == "*" || idToken == "" {
		return st.Append(time.Now(), fields), ""
	}
	// Explicit ID: treat as monotonic millis (best-effort; no strict ordering
	// enforcement to keep the emulator simple and forward-compatible).
	_ = idToken
	return st.Append(time.Now(), fields), ""
}

// handleXGroup implements `XGROUP CREATE key group id [MKSTREAM]`.
func handleXGroup(args []string) string {
	if len(args) < 5 || !strings.EqualFold(args[1], "CREATE") {
		return "wrong number of arguments for 'xgroup' command"
	}
	key := args[2]
	group := args[3]
	startID := args[4]
	if err := redisqueue.Stream(key).CreateGroup(group, startID); err != nil {
		return err.Error()
	}
	return ""
}

// handleXReadGroup implements
// `XREADGROUP GROUP g c [COUNT n] [BLOCK ms] STREAMS key [key...] id [id...]`.
// Only the first stream is supported. Returns (key, entries, errMsg).
func handleXReadGroup(args []string) (string, []redisqueue.StreamEntry, string) {
	if len(args) < 6 {
		return "", nil, "wrong number of arguments for 'xreadgroup' command"
	}
	// GROUP g c
	group := args[2]
	consumer := args[3]

	count := 1
	idx := 4
	for idx < len(args) {
		switch strings.ToUpper(args[idx]) {
		case "COUNT":
			if idx+1 < len(args) {
				if n, err := strconv.Atoi(args[idx+1]); err == nil && n > 0 {
					count = n
				}
				idx += 2
				continue
			}
			return "", nil, "wrong number of arguments for 'xreadgroup' command"
		case "BLOCK":
			// Block is advisory in the in-process emulator; ignore the value.
			if idx+1 < len(args) {
				idx += 2
				continue
			}
			return "", nil, "wrong number of arguments for 'xreadgroup' command"
		case "STREAMS":
			idx++
			if idx >= len(args) || idx+1 >= len(args) {
				return "", nil, "wrong number of arguments for 'xreadgroup' command"
			}
			key := args[idx]
			_ = args[idx+1] // start id: only ">" (new entries) is meaningful here
			entries := redisqueue.Stream(key).ReadGroup(group, consumer, count)
			return key, entries, ""
		default:
			return "", nil, "wrong number of arguments for 'xreadgroup' command"
		}
	}
	return "", nil, "wrong number of arguments for 'xreadgroup' command"
}

// handleXAck implements `XACK key group id [id...]`. Returns (ackedCount, errMsg).
func handleXAck(args []string) (int, string) {
	if len(args) < 4 {
		return 0, "wrong number of arguments for 'xack' command"
	}
	key := args[1]
	group := args[2]
	ids := args[3:]
	before := redisqueue.Stream(key).PendingCount(group)
	redisqueue.Stream(key).Ack(group, ids)
	after := redisqueue.Stream(key).PendingCount(group)
	return before - after, ""
}

// writeStreamReadResult writes an XREADGROUP response:
// `*1 *2 $key *N ($id *2 $field $value ...)...`.
func writeStreamReadResult(writer *bufio.Writer, key string, entries []redisqueue.StreamEntry) error {
	if errWrite := writeRedisArrayHeader(writer, 1); errWrite != nil {
		return errWrite
	}
	if errWrite := writeRedisArrayHeader(writer, 2); errWrite != nil {
		return errWrite
	}
	if errWrite := writeRedisBulkString(writer, []byte(key)); errWrite != nil {
		return errWrite
	}
	if errWrite := writeRedisArrayHeader(writer, len(entries)); errWrite != nil {
		return errWrite
	}
	for _, entry := range entries {
		if errWrite := writeRedisArrayHeader(writer, 2); errWrite != nil {
			return errWrite
		}
		if errWrite := writeRedisBulkString(writer, []byte(entry.ID)); errWrite != nil {
			return errWrite
		}
		// Field-value pairs.
		if errWrite := writeRedisArrayHeader(writer, len(entry.Fields)*2); errWrite != nil {
			return errWrite
		}
		for field, value := range entry.Fields {
			if errWrite := writeRedisBulkString(writer, []byte(field)); errWrite != nil {
				return errWrite
			}
			if errWrite := writeRedisBulkString(writer, []byte(value)); errWrite != nil {
				return errWrite
			}
		}
	}
	return nil
}
