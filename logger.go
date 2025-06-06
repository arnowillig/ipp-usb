/* ipp-usb - HTTP reverse proxy, backed by IPP-over-USB connection to device
 *
 * Copyright (C) 2020 and up by Alexander Pevzner (pzz@apevzner.com)
 * See LICENSE for license terms and conditions
 *
 * Logging
 */

package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OpenPrinting/goipp"
)

const (
	// LogMinFileSize specifies a minimum value for the
	// max-file-size parameter
	LogMinFileSize = 16 * 1024
)

// Standard loggers
var (
	// This is the default logger
	Log = NewLogger().ToMainFile()

	// Console logger always writes to console
	Console = NewLogger().ToConsole()

	// Initlog used only on initialization time
	// It writes to Stdout or Stderr, depending
	// on log level
	InitLog = NewLogger().ToStdOutErr()
)

// LogLevel enumerates possible log levels
type LogLevel int

// LogLevel constants
const (
	LogError LogLevel = 1 << iota
	LogInfo
	LogDebug
	LogTraceIPP
	LogTraceESCL
	LogTraceHTTP
	LogTraceUSB

	LogAll      = LogError | LogInfo | LogDebug | LogTraceAll
	LogTraceAll = LogTraceIPP | LogTraceESCL | LogTraceHTTP | LogTraceUSB
)

// Adjust LogLevel mask, so more detailed log levels
// imply less detailed
func (levels *LogLevel) Adjust() {
	switch {
	case *levels&LogTraceAll != 0:
		*levels |= LogDebug | LogInfo | LogError
	case *levels&LogDebug != 0:
		*levels |= LogInfo | LogError
	case *levels&LogInfo != 0:
		*levels |= LogError
	}
}

// loggerMode enumerates possible Logger modes
type loggerMode int

const (
	loggerNoMode       loggerMode = iota // Mode not yet set; log is buffered
	loggerDiscard                        // Log goes to nowhere
	loggerConsole                        // Log goes to console
	loggerColorConsole                   // Log goes to console and uses ANSI colors
	loggerFile                           // Log goes to disk file
)

// Logger implements logging facilities
type Logger struct {
	LogMessage                 // "Root" log message
	levels     LogLevel        // Levels generated by this logger
	ccLevels   LogLevel        // Sum of Cc's levels
	paused     int32           // Logger paused, if counter > 0
	mode       loggerMode      // Logger mode
	lock       sync.Mutex      // Write lock
	path       string          // Path to log file
	cc         []*Logger       // Loggers to send carbon copy to
	out        io.Writer       // Output stream, may be *os.File
	outhook    func(io.Writer, // Output hook
		LogLevel, []byte)

	// Don't reexport these methods from the root message
	Commit, Reject struct{}
}

// NewLogger creates new logger. Logger mode is not set,
// so logs written to this logger a buffered until mode
// (and direction) is set
func NewLogger() *Logger {
	l := &Logger{
		mode:     loggerNoMode,
		levels:   LogAll,
		ccLevels: 0,
		outhook: func(w io.Writer, _ LogLevel, line []byte) {
			w.Write(line)
		},
	}

	l.LogMessage.logger = l

	return l
}

// ToNowhere redirects log to nowhere
func (l *Logger) ToNowhere() *Logger {
	l.mode = loggerDiscard
	l.out = ioutil.Discard
	return l
}

// ToConsole redirects log to console
func (l *Logger) ToConsole() *Logger {
	l.mode = loggerConsole
	l.out = os.Stdout
	return l
}

// ToColorConsole redirects log to console with ANSI colors
func (l *Logger) ToColorConsole() *Logger {
	if logIsAtty(os.Stdout) {
		l.outhook = logColorConsoleWrite
	}

	return l.ToConsole()
}

// ToStdOutErr redirects log to Stdout or Stderr, depending
// on LogLevel
func (l *Logger) ToStdOutErr() *Logger {
	l.outhook = func(out io.Writer, level LogLevel, line []byte) {
		if level == LogError {
			out = os.Stderr
		}
		out.Write(line)
	}

	return l.ToConsole()
}

// ToFile redirects log to arbitrary log file
func (l *Logger) ToFile(path string) *Logger {
	l.path = path
	l.mode = loggerFile
	l.out = nil // Will be opened on demand
	return l
}

// ToMainFile redirects log to the main log file
func (l *Logger) ToMainFile() *Logger {
	return l.ToFile(filepath.Join(PathLogDir, "main.log"))
}

// ToDevFile redirects log to per-device log file
func (l *Logger) ToDevFile(info UsbDeviceInfo) *Logger {
	return l.ToFile(filepath.Join(PathLogDir, info.Ident()+".log"))
}

// HasDestination reports if Logger destination is already
// configured (i.e., Logger.ToXXX called for this logger).
func (l *Logger) HasDestination() bool {
	return l.mode != loggerNoMode
}

// Cc adds Logger to send "carbon copy" to.
func (l *Logger) Cc(to *Logger) *Logger {
	l.cc = append(l.cc, to)
	l.ccLevels |= to.levels

	return l
}

// Close the logger
func (l *Logger) Close() {
	if l.mode == loggerFile && l.out != nil {
		if file, ok := l.out.(*os.File); ok {
			file.Close()
		}
	}
}

// SetLevels set logger's log levels
func (l *Logger) SetLevels(levels LogLevel) *Logger {
	levels.Adjust()
	l.levels = levels
	return l
}

// Pause the logger. All output will be buffered,
// and flushed to destination when logger is resumed
func (l *Logger) Pause() *Logger {
	atomic.AddInt32(&l.paused, 1)
	return l
}

// Resume the logger. All buffered output will be
// flushed
func (l *Logger) Resume() *Logger {
	if atomic.AddInt32(&l.paused, -1) == 0 {
		l.LogMessage.Flush()
	}
	return l
}

// Panic writes to log a panic message, including
// call stack, and terminates a program
func (l *Logger) Panic(v interface{}) {
	l.Error('!', "panic: %v", v)
	l.Error('!', "")

	w := l.LineWriter(LogError, '!')
	w.Write(debug.Stack())
	w.Close()

	os.Exit(1)
}

// Format a time prefix
func (l *Logger) fmtTime() *logLineBuf {
	buf := logLineBufAlloc(0, 0)

	if l.mode == loggerFile {
		now := time.Now()

		year, month, day := now.Date()
		hour, min, sec := now.Clock()

		fmt.Fprintf(buf, "%2.2d-%2.2d-%4.4d %2.2d:%2.2d:%2.2d:",
			day, month, year,
			hour, min, sec)
	}

	return buf
}

// Handle log rotation
func (l *Logger) rotate() {
	// Do we need to rotate?
	file, ok := l.out.(*os.File)
	if !ok {
		return
	}

	stat, err := file.Stat()
	if err != nil || stat.Size() <= Conf.LogMaxFileSize {
		return
	}

	// Perform rotation
	if Conf.LogMaxBackupFiles > 0 {
		prevpath := ""
		for i := Conf.LogMaxBackupFiles; i > 0; i-- {
			nextpath := fmt.Sprintf("%s.%d.gz", l.path, i-1)

			if i == Conf.LogMaxBackupFiles {
				os.Remove(nextpath)
			} else {
				os.Rename(nextpath, prevpath)
			}

			prevpath = nextpath
		}

		err := l.gzip(l.path, prevpath)
		if err != nil {
			return
		}
	}

	file.Truncate(0)
}

// gzip the log file
func (l *Logger) gzip(ipath, opath string) error {
	// Open input file
	ifile, err := os.Open(ipath)
	if err != nil {
		return err
	}

	defer ifile.Close()

	// Open output file
	ofile, err := os.OpenFile(opath, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0644)
	if err != nil {
		return err
	}

	// gzip ifile->ofile
	w := gzip.NewWriter(ofile)
	_, err = io.Copy(w, ifile)
	err2 := w.Close()
	err3 := ofile.Close()

	switch {
	case err == nil && err2 != nil:
		err = err2
	case err == nil && err3 != nil:
		err = err3
	}

	// Cleanup and exit
	if err != nil {
		os.Remove(opath)
	}

	return err
}

// LogMessage represents a single (possible multi line) log
// message, which will appear in the output log atomically,
// and will be not interrupted in the middle by other log activity
type LogMessage struct {
	logger *Logger       // Underlying logger
	parent *LogMessage   // Parent message
	lines  []*logLineBuf // One buffer per line
}

// logMessagePool manages a pool of reusable LogMessages
var logMessagePool = sync.Pool{New: func() interface{} { return &LogMessage{} }}

// Begin returns a child (nested) LogMessage. Writes to this
// child message appended to the parent message
func (msg *LogMessage) Begin() *LogMessage {
	msg2 := logMessagePool.Get().(*LogMessage)
	msg2.logger = msg.logger
	msg2.parent = msg
	return msg2
}

// Add formats a next line of log message, with level and prefix char
func (msg *LogMessage) Add(level LogLevel, prefix byte,
	format string, args ...interface{}) *LogMessage {

	if (msg.logger.levels|msg.logger.ccLevels)&level != 0 {
		buf := logLineBufAlloc(level, prefix)
		fmt.Fprintf(buf, format, args...)

		msg.appendLineBuf(buf)
	}

	return msg
}

// Nl adds empty line to the log message
func (msg *LogMessage) Nl(level LogLevel) *LogMessage {
	return msg.Add(level, ' ', "")
}

// addBytes adds a next line of log message, taking slice of bytes as input
func (msg *LogMessage) addBytes(level LogLevel, prefix byte, line []byte) *LogMessage {
	if (msg.logger.levels|msg.logger.ccLevels)&level != 0 {
		buf := logLineBufAlloc(level, prefix)
		buf.Write(line)

		msg.appendLineBuf(buf)
	}

	return msg
}

// appendLineBuf appends line buffer to msg.lines
func (msg *LogMessage) appendLineBuf(buf *logLineBuf) {
	if msg.parent == nil {
		// Note, many threads may write to the root
		// message simultaneously
		msg.logger.lock.Lock()
		msg.lines = append(msg.lines, buf)
		msg.logger.lock.Unlock()

		msg.Flush()
	} else {
		msg.lines = append(msg.lines, buf)
	}
}

// Debug appends a LogDebug line to the message
func (msg *LogMessage) Debug(prefix byte, format string, args ...interface{}) *LogMessage {
	return msg.Add(LogDebug, prefix, format, args...)
}

// Info appends a LogInfo line to the message
func (msg *LogMessage) Info(prefix byte, format string, args ...interface{}) *LogMessage {
	return msg.Add(LogInfo, prefix, format, args...)
}

// Error appends a LogError line to the message
func (msg *LogMessage) Error(prefix byte, format string, args ...interface{}) *LogMessage {
	return msg.Add(LogError, prefix, format, args...)
}

// Exit appends a LogError line to the message, flushes the message and
// all its parents and terminates a program by calling os.Exit(1)
func (msg *LogMessage) Exit(prefix byte, format string, args ...interface{}) {
	if msg.logger.mode == loggerNoMode {
		msg.logger.ToConsole()
	}

	msg.Error(prefix, format, args...)
	for msg.parent != nil {
		msg.Flush()
		msg = msg.parent
	}
	os.Exit(1)
}

// Check calls msg.Exit(), if err is not nil
func (msg *LogMessage) Check(err error) {
	if err != nil {
		msg.Exit(0, "%s", err)
	}
}

// HexDump appends a HEX dump to the log message
func (msg *LogMessage) HexDump(level LogLevel, prefix byte,
	data []byte) *LogMessage {

	if (msg.logger.levels|msg.logger.ccLevels)&level == 0 {
		return msg
	}

	hex := logLineBufAlloc(0, 0)
	chr := logLineBufAlloc(0, 0)

	defer hex.free()
	defer chr.free()

	off := 0

	for len(data) > 0 {
		hex.Reset()
		chr.Reset()

		sz := len(data)
		if sz > 16 {
			sz = 16
		}

		i := 0
		for ; i < sz; i++ {
			c := data[i]
			fmt.Fprintf(hex, "%2.2x", data[i])
			if i%4 == 3 {
				hex.Write([]byte(":"))
			} else {
				hex.Write([]byte(" "))
			}

			if 0x20 <= c && c < 0x80 {
				chr.WriteByte(c)
			} else {
				chr.WriteByte('.')
			}
		}

		for ; i < 16; i++ {
			hex.WriteString("   ")
		}

		msg.Add(level, prefix, "%4.4x: %s %s", off, hex, chr)

		off += sz
		data = data[sz:]
	}

	return msg
}

// HTTPRequest dumps HTTP request (except body) to the log message
func (msg *LogMessage) HTTPRequest(level LogLevel, prefix byte,
	session int, rq *http.Request) *LogMessage {

	if (msg.logger.levels|msg.logger.ccLevels)&level == 0 {
		return msg
	}

	// Clone request, drop body
	rq = rq.WithContext(context.Background())
	rq.Body = struct{ io.ReadCloser }{http.NoBody}

	// Write it to the log
	msg.Add(level, prefix, "HTTP[%3.3d]: HTTP request header:", session)

	buf := &bytes.Buffer{}
	rq.Write(buf)

	for _, l := range bytes.Split(buf.Bytes(), []byte("\n")) {
		if sz := len(l); sz > 0 && l[sz-1] == '\r' {
			l = l[:sz-1]
		}

		msg.Add(level, prefix, "  %s", l)

		if len(l) == 0 {
			break
		}
	}

	return msg
}

// HTTPResponse dumps HTTP response (expect body) to the log message
func (msg *LogMessage) HTTPResponse(level LogLevel, prefix byte,
	session int, rsp *http.Response) *LogMessage {

	if (msg.logger.levels|msg.logger.ccLevels)&level == 0 {
		return msg
	}

	// Clone response header. Avoid rsp.Header.Clone(),
	// because Go 11 doesn't support it yet
	hdr := make(http.Header, len(rsp.Header))
	for k, v := range rsp.Header {
		hdr[k] = v
	}

	// Go stdlib strips Transfer-Encoding header, so reconstruct it
	if rsp.TransferEncoding != nil {
		hdr.Add("Transfer-Encoding",
			strings.Join(rsp.TransferEncoding, ", "))
	}

	// Write it to the log
	msg.Add(level, prefix, "HTTP[%3.3d]: HTTP response header:", session)
	msg.Add(level, prefix, "  %s %s", rsp.Proto, rsp.Status)

	keys := make([]string, 0, len(hdr))

	for k := range hdr {
		keys = append(keys, k)
	}

	sort.Strings(keys)
	for _, k := range keys {
		msg.Add(level, prefix, "  %s: %s", k, hdr.Get(k))
	}

	msg.Add(level, prefix, "  ")

	return msg
}

// HTTPRqParams dumps HTTP request parameters into the log message
func (msg *LogMessage) HTTPRqParams(level LogLevel, prefix byte,
	session int, rq *http.Request) *LogMessage {

	msg.Add(level, prefix, "HTTP[%3.3d]: %s %s", session, rq.Method, rq.URL)

	return msg
}

// HTTPRspStatus dumps HTTP response status into the log message
func (msg *LogMessage) HTTPRspStatus(level LogLevel, prefix byte,
	session int, rq *http.Request, rsp *http.Response) *LogMessage {

	msg.Add(level, prefix, "HTTP[%3.3d]: %s %s - %s",
		session, rq.Method, rq.URL, rsp.Status)

	return msg
}

// HTTPError writes HTTP error into the log message
func (msg *LogMessage) HTTPError(prefix byte,
	session int, format string, args ...interface{}) *LogMessage {

	msg.Error(prefix, "HTTP[%3.3d]: %s", session, fmt.Sprintf(format, args...))

	return msg
}

// HTTPDebug writes HTTP debug line into the log message
func (msg *LogMessage) HTTPDebug(prefix byte,
	session int, format string, args ...interface{}) *LogMessage {

	msg.Debug(prefix, "HTTP[%3.3d]: %s", session, fmt.Sprintf(format, args...))

	return msg
}

// IppRequest dumps IPP request into the log message
func (msg *LogMessage) IppRequest(level LogLevel, prefix byte,
	m *goipp.Message) *LogMessage {

	if (msg.logger.levels|msg.logger.ccLevels)&level != 0 {
		m.Print(msg.LineWriter(level, prefix), true)
	}
	return msg
}

// IppResponse dumps IPP response into the log message
func (msg *LogMessage) IppResponse(level LogLevel, prefix byte,
	m *goipp.Message) *LogMessage {

	if (msg.logger.levels|msg.logger.ccLevels)&level != 0 {
		m.Print(msg.LineWriter(level, prefix), false)
	}
	return msg
}

// LineWriter creates a LineWriter that writes to the LogMessage,
// using specified LogLevel and prefix
func (msg *LogMessage) LineWriter(level LogLevel, prefix byte) *LineWriter {
	return &LineWriter{
		Func: func(line []byte) { msg.addBytes(level, prefix, line) },
	}
}

// Commit message to the log
func (msg *LogMessage) Commit() {
	msg.Flush()
	msg.free()
}

// Flush message content to the log
//
// This is equal to committing the message and starting
// the new message, with the exception that old message
// pointer remains valid. Message logical atomicity is not
// preserved between flushes
func (msg *LogMessage) Flush() {
	// Lock the logger
	msg.logger.lock.Lock()
	defer msg.logger.lock.Unlock()

	// Ignore empty messages
	if len(msg.lines) == 0 {
		return
	}

	// If message has a parent, simply flush our content there
	if msg.parent != nil {
		msg.parent.lines = append(msg.parent.lines, msg.lines...)
		msg.lines = msg.lines[:0]

		// If our parent is root, we need to flush root as well
		if msg.parent.parent == nil {
			msg = msg.parent
		} else {
			return
		}
	}

	// Do nothing, if logger is paused
	if atomic.LoadInt32(&msg.logger.paused) != 0 {
		return
	}

	// Open log file on demand
	if msg.logger.out == nil && msg.logger.mode == loggerFile {
		MakeParentDirectory(msg.logger.path)
		msg.logger.out, _ = os.OpenFile(msg.logger.path,
			os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0644)
	}

	// If there are still no destination, return for now.
	//
	// It may happen either because destination is not yet
	// set (log.ToXXX not called) or because we've failed
	// to open destination file.
	//
	// In both cases if we return now, the logger will continue
	// to work in the buffering mode, which is desired behavior.
	if msg.logger.out == nil {
		return
	}

	// Rotate now
	if msg.logger.mode == loggerFile {
		msg.logger.rotate()
	}

	// Prepare to carbon-copy
	var cclist []struct {
		levels LogLevel
		msg    *LogMessage
	}

	for _, cc := range msg.logger.cc {
		cclist = append(cclist, struct {
			levels LogLevel
			msg    *LogMessage
		}{cc.levels, cc.Begin()})
	}

	// Send message content to the logger
	buf := msg.logger.fmtTime()
	defer buf.free()

	timeLen := buf.Len()
	for _, l := range msg.lines {
		l.trim()

		// Generate own output
		buf.Truncate(timeLen)
		if l.level&msg.logger.levels != 0 {
			if !l.empty() {
				if timeLen != 0 {
					buf.WriteByte(' ')
				}

				buf.Write(l.Bytes())
			}

			buf.WriteByte('\n')
			msg.logger.outhook(msg.logger.out, l.level, buf.Bytes())
		}

		// Send carbon copies
		for _, cc := range cclist {
			if (cc.levels & l.level) != 0 {
				cc.msg.addBytes(l.level, 0, l.Bytes())
			}
		}

		l.free()
	}

	// Commit carbon copies
	for _, cc := range cclist {
		cc.msg.Commit()
	}

	// Reset the message
	msg.lines = msg.lines[:0]
}

// Reject the message
func (msg *LogMessage) Reject() {
	msg.free()
}

// Return message to the logMessagePool
func (msg *LogMessage) free() {
	// Free all lines
	for _, l := range msg.lines {
		l.free()
	}

	// Reset the message and put it to the pool
	if len(msg.lines) < 16 {
		msg.lines = msg.lines[:0] // Keep memory, reset content
	} else {
		msg.lines = nil // Drop this large buffer
	}

	msg.logger = nil

	logMessagePool.Put(msg)
}

// logLineBuf represents a single log line buffer
type logLineBuf struct {
	bytes.Buffer          // Underlying buffer
	level        LogLevel // Log level the line was written on
}

// logLinePool manages a pool of reusable logLines
var logLineBufPool = sync.Pool{New: func() interface{} {
	return &logLineBuf{
		Buffer: bytes.Buffer{},
	}
}}

// logLineAlloc() allocates a logLineBuf
func logLineBufAlloc(level LogLevel, prefix byte) *logLineBuf {
	buf := logLineBufPool.Get().(*logLineBuf)
	buf.level = level
	if prefix != 0 {
		buf.Write([]byte{prefix, ' '})
	}
	return buf
}

// free returns the logLineBuf to the pool
func (buf *logLineBuf) free() {
	if buf.Cap() <= 256 {
		buf.Reset()
		logLineBufPool.Put(buf)
	}
}

// trim removes trailing spaces
func (buf *logLineBuf) trim() {
	bytes := buf.Bytes()
	var i int

loop:
	for i = len(bytes); i > 0; i-- {
		c := bytes[i-1]
		switch c {
		case '\t', '\n', '\v', '\f', '\r', ' ', 0x85, 0xA0:
		default:
			break loop
		}
	}
	buf.Truncate(i)
}

// empty returns true if logLineBuf is empty (no text, no prefix)
func (buf *logLineBuf) empty() bool {
	return buf.Len() == 0
}
