package main

import (
	"fmt"
	"os"
	"time"
)

// setupLogging creates the logs directory and initializes log files
func (tui *TUI) setupLogging() error {
	// Create logs directory if it doesn't exist
	if err := os.MkdirAll("logs", 0755); err != nil {
		return fmt.Errorf("failed to create logs directory: %v", err)
	}

	// Set current date and open initial log files
	tui.currentDate = time.Now().Format("2006-01-02")
	return tui.openLogFiles()
}

// openLogFiles opens or creates the daily log files
func (tui *TUI) openLogFiles() error {
	tui.fileMutex.Lock()
	defer tui.fileMutex.Unlock()

	// Close existing files if open
	if tui.chatLogFile != nil {
		tui.chatLogFile.Close()
	}
	if tui.rawLogFile != nil {
		tui.rawLogFile.Close()
	}

	// Open chat log file
	chatLogPath := fmt.Sprintf("logs/%s_chat_log.txt", tui.currentDate)
	chatFile, err := os.OpenFile(chatLogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open chat log file: %v", err)
	}
	tui.chatLogFile = chatFile

	// Open raw log file
	rawLogPath := fmt.Sprintf("logs/%s_raw_log.txt", tui.currentDate)
	rawFile, err := os.OpenFile(rawLogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		chatFile.Close() // Close chat file if raw file fails
		return fmt.Errorf("failed to open raw log file: %v", err)
	}
	tui.rawLogFile = rawFile

	// Write session start markers
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	sessionStart := fmt.Sprintf("\n=== SESSION START: %s ===\n", timestamp)

	if _, err := tui.chatLogFile.WriteString(sessionStart); err == nil {
		tui.chatLogFile.Sync()
	}
	if _, err := tui.rawLogFile.WriteString(sessionStart); err == nil {
		tui.rawLogFile.Sync()
	}

	return nil
}

// checkDateRotation checks if we need to rotate to new log files for a new day
func (tui *TUI) checkDateRotation() {
	currentDate := time.Now().Format("2006-01-02")
	if currentDate != tui.currentDate {
		// Write session end to old files
		timestamp := time.Now().Format("2006-01-02 15:04:05")
		sessionEnd := fmt.Sprintf("=== SESSION END: %s ===\n", timestamp)

		tui.fileMutex.Lock()
		if tui.chatLogFile != nil {
			tui.chatLogFile.WriteString(sessionEnd)
			tui.chatLogFile.Sync()
		}
		if tui.rawLogFile != nil {
			tui.rawLogFile.WriteString(sessionEnd)
			tui.rawLogFile.Sync()
		}
		tui.fileMutex.Unlock()

		// Update date and open new files
		tui.currentDate = currentDate
		if err := tui.openLogFiles(); err != nil {
			// If we can't open new files, add error to current log display
			tui.AddLogMessage(fmt.Sprintf("❌ Failed to rotate log files: %v", err))
		} else {
			tui.AddLogMessage("📁 Rotated to new daily log files")
		}
	}
}

// writeToChatLog writes a formatted message to the chat log file
func (tui *TUI) writeToChatLog(timestamp, username, message string) {
	tui.checkDateRotation()

	tui.fileMutex.Lock()
	defer tui.fileMutex.Unlock()

	if tui.chatLogFile == nil {
		return
	}

	timeStr := timestamp
	if timeStr == "" {
		timeStr = time.Now().Format("15:04:05")
	}
	fullTimestamp := time.Now().Format("2006-01-02 15:04:05")

	var logEntry string
	if username != "" {
		logEntry = fmt.Sprintf("[%s] <%s> %s\n", fullTimestamp, username, message)
	} else {
		logEntry = fmt.Sprintf("[%s] *** %s\n", fullTimestamp, message)
	}

	if _, err := tui.chatLogFile.WriteString(logEntry); err == nil {
		tui.chatLogFile.Sync() // Force write to disk
	}
}

// writeToRawLog writes a system log message to the raw log file
func (tui *TUI) writeToRawLog(message string) {
	tui.checkDateRotation()

	tui.fileMutex.Lock()
	defer tui.fileMutex.Unlock()

	if tui.rawLogFile == nil {
		return
	}

	timestamp := time.Now().Format("2006-01-02 15:04:05.000")
	logEntry := fmt.Sprintf("[%s] %s\n", timestamp, message)

	if _, err := tui.rawLogFile.WriteString(logEntry); err == nil {
		tui.rawLogFile.Sync() // Force write to disk
	}
}

// closeLogFiles safely closes all log files
func (tui *TUI) closeLogFiles() {
	tui.fileMutex.Lock()
	defer tui.fileMutex.Unlock()

	timestamp := time.Now().Format("2006-01-02 15:04:05")
	sessionEnd := fmt.Sprintf("=== SESSION END: %s ===\n\n", timestamp)

	if tui.chatLogFile != nil {
		tui.chatLogFile.WriteString(sessionEnd)
		tui.chatLogFile.Sync()
		tui.chatLogFile.Close()
		tui.chatLogFile = nil
	}

	if tui.rawLogFile != nil {
		tui.rawLogFile.WriteString(sessionEnd)
		tui.rawLogFile.Sync()
		tui.rawLogFile.Close()
		tui.rawLogFile = nil
	}
}
