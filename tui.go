package main

import (
	"fmt"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// NewTUI creates a new terminal user interface
func NewTUI() *TUI {
	tui := &TUI{
		app:   tview.NewApplication(),
		users: make(map[string]bool),
		// Initialize input history
		inputHistory: make([]string, 0),
		historyIndex: 0,
		currentInput: "",
	}

	// Setup logging first
	if err := tui.setupLogging(); err != nil {
		fmt.Printf("❌ Failed to setup logging: %v\n", err)
		// Continue anyway, just without file logging
	} else {
		fmt.Println("📁 Logging initialized - saving to logs/ directory")
	}

	// Create the main components
	tui.chatView = tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true).
		SetWrap(true).
		SetChangedFunc(func() {
			tui.app.Draw()
		})
	tui.chatView.SetBorder(true).SetTitle(" Chat Messages ").SetTitleAlign(tview.AlignLeft)

	tui.logView = tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true).
		SetWrap(true).
		SetChangedFunc(func() {
			tui.app.Draw()
		})
	tui.logView.SetBorder(true).SetTitle(" System Log ").SetTitleAlign(tview.AlignLeft)

	tui.userView = tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true).
		SetChangedFunc(func() {
			tui.app.Draw()
		})
	tui.userView.SetBorder(true).SetTitle(" Users ").SetTitleAlign(tview.AlignLeft)

	tui.inputField = tview.NewInputField().
		SetLabel("> ").
		SetFieldWidth(0).
		SetFieldBackgroundColor(tcell.ColorBlack).
		SetFieldTextColor(tcell.ColorWhite)
	tui.inputField.SetBorder(true).SetTitle(" Input ").SetTitleAlign(tview.AlignLeft)

	tui.statusBar = tview.NewTextView().
		SetDynamicColors(true).
		SetText("Starting Skyskraber Chat Bot...")
	tui.statusBar.SetBorder(true).SetTitle(" Status ").SetTitleAlign(tview.AlignLeft)

	// Create layout
	mainContent := tview.NewFlex().SetDirection(tview.FlexColumn).
		AddItem(tui.chatView, 0, 3, false).      // Left pane (chat) - 3/5 of width
		AddItem(tui.logView, 0, 2, false).       // Right pane (logs) - 2/5 of width
		AddItem(tui.userView, 20, 0, false)      // Most right pane (users) - fixed 20 chars

	bottomSection := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(tui.inputField, 3, 0, true).     // Input field - 3 lines high
		AddItem(tui.statusBar, 3, 0, false)      // Status bar - 3 lines high

	tui.layout = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(mainContent, 0, 1, false).       // Main content area
		AddItem(bottomSection, 6, 0, true)       // Bottom section - 6 lines total

	// Enable mouse support
	tui.app.EnableMouse(true)
	tui.app.SetRoot(tui.layout, true)

	return tui
}

// AddChatMessage adds a formatted chat message to the chat pane
func (tui *TUI) AddChatMessage(timestamp, username, message string) {
	// Write to log file first
	tui.writeToChatLog(timestamp, username, message)

	// Then update the UI
	tui.app.QueueUpdateDraw(func() {
		tui.chatMutex.Lock()
		defer tui.chatMutex.Unlock()

		timeStr := timestamp
		if timeStr == "" {
			timeStr = time.Now().Format("15:04:05")
		}

		var formattedMsg string
		if username != "" {
			formattedMsg = fmt.Sprintf("[gray]%s[white] [yellow]<%s>[white] %s\n", timeStr, username, message)
		} else {
			formattedMsg = fmt.Sprintf("[gray]%s[white] [blue]***[white] %s\n", timeStr, message)
		}

		fmt.Fprint(tui.chatView, formattedMsg)
		tui.chatView.ScrollToEnd()
	})
}

// AddLogMessage adds a log message to the log pane
func (tui *TUI) AddLogMessage(message string) {
	// Write to log file first
	tui.writeToRawLog(message)

	// Then update the UI
	tui.app.QueueUpdateDraw(func() {
		tui.logMutex.Lock()
		defer tui.logMutex.Unlock()

		timeStr := time.Now().Format("15:04:05.000")
		formattedMsg := fmt.Sprintf("[gray]%s[white] %s\n", timeStr, message)

		fmt.Fprint(tui.logView, formattedMsg)
		tui.logView.ScrollToEnd()
	})
}

// UpdateUsers updates the user list in the user pane
func (tui *TUI) UpdateUsers(users []string) {
	tui.app.QueueUpdateDraw(func() {
		tui.userMutex.Lock()
		defer tui.userMutex.Unlock()

		tui.userView.Clear()
		fmt.Fprintf(tui.userView, "[yellow]Room: %s[white]\n\n", tui.currentRoom)

		for _, user := range users {
			fmt.Fprintf(tui.userView, "[green]● [white]%s\n", user)
		}
	})
}

// AddUser adds a user to the current room
func (tui *TUI) AddUser(username string) {
	tui.userMutex.Lock()
	tui.users[username] = true
	users := make([]string, 0, len(tui.users))
	for user := range tui.users {
		users = append(users, user)
	}
	tui.userMutex.Unlock()

	tui.UpdateUsers(users)
}

// RemoveUser removes a user from the current room
func (tui *TUI) RemoveUser(username string) {
	tui.userMutex.Lock()
	delete(tui.users, username)
	users := make([]string, 0, len(tui.users))
	for user := range tui.users {
		users = append(users, user)
	}
	tui.userMutex.Unlock()

	tui.UpdateUsers(users)
}

// SetRoom updates the current room and clears users
func (tui *TUI) SetRoom(roomID, roomName string) {
	tui.userMutex.Lock()
	tui.currentRoom = fmt.Sprintf("%s (%s)", roomName, roomID)
	tui.users = make(map[string]bool) // Clear users when changing rooms
	tui.userMutex.Unlock()

	tui.UpdateUsers([]string{})
}

// SetRoomUsers sets the initial user list for a room
func (tui *TUI) SetRoomUsers(usernames []string) {
	tui.userMutex.Lock()
	tui.users = make(map[string]bool)
	for _, username := range usernames {
		tui.users[username] = true
	}
	tui.userMutex.Unlock()

	tui.UpdateUsers(usernames)
}

// UpdateOnlineTime updates the tracked online time from server data
func (tui *TUI) UpdateOnlineTime(hours, minutes int) {
	tui.onlineTimeMutex.Lock()
	tui.onlineHours = hours
	tui.onlineMinutes = minutes
	tui.onlineTimeMutex.Unlock()
}

// GetOnlineTime returns the current online time
func (tui *TUI) GetOnlineTime() (int, int) {
	tui.onlineTimeMutex.RLock()
	defer tui.onlineTimeMutex.RUnlock()
	return tui.onlineHours, tui.onlineMinutes
}

// UpdateStatusBar updates the comprehensive status bar with all information
func (tui *TUI) UpdateStatusBar(username string, connected bool, roomName, roomID, autopickStatus string) {
	tui.app.QueueUpdateDraw(func() {
		currentTime := time.Now().Format("15:04:05")

		var connStatus string
		if connected {
			connStatus = fmt.Sprintf("Connected as %s", username)
		} else {
			connStatus = "DISCONNECTED"
		}

		hours, minutes := tui.GetOnlineTime()
		onlineTime := fmt.Sprintf("Online: %dh %dm", hours, minutes)

		var roomInfo string
		if roomName != "" && roomID != "" && roomName != "Unknown" {
			roomInfo = fmt.Sprintf("Room: %s (%s)", roomName, roomID)
		} else {
			roomInfo = fmt.Sprintf("Room: %s", roomID)
		}

		statusText := fmt.Sprintf("[yellow]%s[white] | [green]%s[white] | [cyan]%s[white] | [blue]%s[white] | [magenta]Autopick: %s[white]",
			currentTime, connStatus, onlineTime, roomInfo, autopickStatus)

		tui.statusBar.Clear()
		fmt.Fprint(tui.statusBar, statusText)
	})
}

// AddToInputHistory adds a command/message to input history
func (tui *TUI) AddToInputHistory(input string) {
	tui.historyMutex.Lock()
	defer tui.historyMutex.Unlock()

	// Don't add empty strings or duplicates of the last entry
	if input == "" || (len(tui.inputHistory) > 0 && tui.inputHistory[len(tui.inputHistory)-1] == input) {
		return
	}

	tui.inputHistory = append(tui.inputHistory, input)

	// Keep history limited to last 50 entries
	if len(tui.inputHistory) > 50 {
		tui.inputHistory = tui.inputHistory[1:]
	}

	// Reset history index
	tui.historyIndex = len(tui.inputHistory)
}

// GetHistoryUp gets the previous command in history
func (tui *TUI) GetHistoryUp() string {
	tui.historyMutex.Lock()
	defer tui.historyMutex.Unlock()

	if len(tui.inputHistory) == 0 {
		return ""
	}

	// Save current input if we're at the end
	if tui.historyIndex >= len(tui.inputHistory) {
		tui.currentInput = tui.inputField.GetText()
	}

	if tui.historyIndex > 0 {
		tui.historyIndex--
	}

	return tui.inputHistory[tui.historyIndex]
}

// GetHistoryDown gets the next command in history
func (tui *TUI) GetHistoryDown() string {
	tui.historyMutex.Lock()
	defer tui.historyMutex.Unlock()

	if len(tui.inputHistory) == 0 {
		return ""
	}

	tui.historyIndex++

	if tui.historyIndex >= len(tui.inputHistory) {
		tui.historyIndex = len(tui.inputHistory)
		return tui.currentInput // Return what they were typing
	}

	return tui.inputHistory[tui.historyIndex]
}

// Run starts the TUI application
func (tui *TUI) Run() error {
	return tui.app.Run()
}

// Stop stops the TUI application
func (tui *TUI) Stop() {
	tui.closeLogFiles()
	tui.app.Stop()
}
