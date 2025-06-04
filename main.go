package main

import (
        "encoding/json"
        "fmt"
        "io"
        "net/http"
        "os"
        "os/signal"
        "strconv"
        "strings"
        "sync"
        "syscall"
        "time"

        "github.com/gdamore/tcell/v2"
        "github.com/gorilla/websocket"
        "github.com/rivo/tview"
)

// Config holds the bot's configuration
type Config struct {
        Username        string   `json:"username"`
        Password        string   `json:"password"`
        IntervalSeconds int      `json:"intervalSeconds"`
        Messages        []string `json:"messages"`
        BaseURL         string   `json:"baseUrl,omitempty"`
        WebSocketURL    string   `json:"webSocketUrl,omitempty"`
}

// TUI handles the terminal user interface
type TUI struct {
        app          *tview.Application
        chatView     *tview.TextView
        logView      *tview.TextView
        userView     *tview.TextView
        inputField   *tview.InputField
        statusBar    *tview.TextView
        layout       *tview.Flex
        chatMutex    sync.Mutex
        logMutex     sync.Mutex
        userMutex    sync.Mutex
        users        map[string]bool
        currentRoom  string
        onlineHours  int
        onlineMinutes int
        onlineTimeMutex sync.RWMutex
        // File logging
        chatLogFile *os.File
        rawLogFile  *os.File
        fileMutex   sync.Mutex
        currentDate string
        // Input history
        inputHistory    []string
        historyIndex    int
        currentInput    string
        historyMutex    sync.Mutex
}

// ChatBot represents the chat bot instance
type ChatBot struct {
        username        string
        password        string
        baseURL         string
        wsURL           string
        httpClient      *http.Client
        wsConn          *websocket.Conn
        cookies         []*http.Cookie
        messageInterval time.Duration
        messages        []string
        currentMsgIndex int
        wsMutex         sync.Mutex
        currentRoom     string
        currentRoomName string
        messageTimer    *time.Timer
        timerMutex      sync.Mutex
        tui             *TUI
        isConnected     bool
        connMutex       sync.RWMutex
        reconnectCount  int
        lastReconnect   time.Time
        // Autopick functionality - updated to use client IDs
        autopickEnabled bool
        autopickMutex   sync.RWMutex
        clientIDToUsername map[int]string    // Track client ID to username mapping
        clientPositions    map[int]Position  // Track positions by client ID
        droppedItems       map[int]DroppedItem // Track dropped items by ID
        ownClientID        int              // Track our own client ID
        ownPosition        Position         // Track our own position
        positionMutex      sync.RWMutex
        clientMutex        sync.RWMutex
        // Room fields
        roomFields      []RoomField        // Available fields in current room
        fieldsMutex     sync.RWMutex
}

// Position represents x,y coordinates
type Position struct {
        X int `json:"x"`
        Y int `json:"y"`
}

// DroppedItem represents an item that was dropped and needs to be picked up
type DroppedItem struct {
        ID               int      `json:"id"`
        Name             string   `json:"name"`
        Position         Position `json:"position"`
        DroppedByClientID int     `json:"droppedByClientID"`
        DroppedByUsername string  `json:"droppedByUsername"`
        DroppedAt        time.Time `json:"droppedAt"`
}

// RoomField represents a field in the room that can potentially be moved to
type RoomField struct {
        X          int    `json:"x"`
        Y          int    `json:"y"`
        State      string `json:"state"`
        IsWalkable bool   `json:"isWalkable"`
}

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

// Run starts the TUI application
func (tui *TUI) Run() error {
        return tui.app.Run()
}

// Stop stops the TUI application
func (tui *TUI) Stop() {
        tui.closeLogFiles()
        tui.app.Stop()
}

// loadConfig loads configuration from a JSON file or creates a template
func loadConfig(configPath string) (*Config, error) {
        defaultConfig := &Config{
                Username:        "CHANGE_THIS_USERNAME",
                Password:        "CHANGE_THIS_PASSWORD",
                IntervalSeconds: 300,
                Messages: []string{
                        "ADD_YOUR_MESSAGES_HERE",
                        "EXAMPLE: Hej alle!",
                        "EXAMPLE: Hvordan har I det?",
                },
                BaseURL:      "https://www.skyskraber.dk",
                WebSocketURL: "wss://www.skyskraber.dk/ws",
        }

        configFile, err := os.ReadFile(configPath)
        if err != nil {
                if os.IsNotExist(err) {
                        configData, errJson := json.MarshalIndent(defaultConfig, "", "  ")
                        if errJson != nil {
                                return nil, fmt.Errorf("failed to marshal template config: %v", errJson)
                        }

                        if errWrite := os.WriteFile(configPath, configData, 0644); errWrite != nil {
                                return nil, fmt.Errorf("failed to create template config file: %v", errWrite)
                        }

                        return defaultConfig, nil
                }
                return nil, fmt.Errorf("failed to read config file: %v", err)
        }

        var config Config
        if errJson := json.Unmarshal(configFile, &config); errJson != nil {
                return nil, fmt.Errorf("failed to parse config file: %v", errJson)
        }

        if config.BaseURL == "" {
                config.BaseURL = "https://www.skyskraber.dk"
        }
        if config.WebSocketURL == "" {
                config.WebSocketURL = "wss://www.skyskraber.dk/ws"
        }

        return &config, nil
}

// NewChatBot creates a new ChatBot instance
func NewChatBot(config *Config, tui *TUI) *ChatBot {
        return &ChatBot{
                username:        config.Username,
                password:        config.Password,
                baseURL:         config.BaseURL,
                wsURL:           config.WebSocketURL,
                httpClient:      &http.Client{Timeout: 30 * time.Second},
                messageInterval: time.Duration(config.IntervalSeconds) * time.Second,
                currentRoom:     "1",
                currentRoomName: "Unknown",
                messages:        config.Messages,
                currentMsgIndex: 0,
                tui:             tui,
                isConnected:     false,
                reconnectCount:  0,
                // Initialize autopick with client ID tracking
                autopickEnabled:    false,
                clientIDToUsername: make(map[int]string),
                clientPositions:    make(map[int]Position),
                droppedItems:       make(map[int]DroppedItem),
                ownClientID:        0,
                ownPosition:        Position{X: 0, Y: 0},
                // Initialize room fields
                roomFields:         make([]RoomField, 0),
        }
}

// log is a helper function to log to the TUI instead of stdout
func (bot *ChatBot) log(format string, args ...interface{}) {
        message := fmt.Sprintf(format, args...)
        if bot.tui != nil {
                bot.tui.AddLogMessage(message)
        }
}

// setConnectionStatus safely updates connection status
func (bot *ChatBot) setConnectionStatus(connected bool) {
        bot.connMutex.Lock()
        defer bot.connMutex.Unlock()

        wasConnected := bot.isConnected
        bot.isConnected = connected

        if connected && !wasConnected {
                bot.reconnectCount = 0
                bot.updateFullStatus()
                bot.log("🟢 Connection status: CONNECTED")
        } else if !connected && wasConnected {
                bot.updateFullStatus()
                bot.log("🔴 Connection status: DISCONNECTED")
        }
}

// Client ID management methods
func (bot *ChatBot) setClientUsername(clientID int, username string) {
        bot.clientMutex.Lock()
        defer bot.clientMutex.Unlock()
        bot.clientIDToUsername[clientID] = username
        
        // If this is our username, store our client ID
        if username == bot.username {
                bot.ownClientID = clientID
                bot.log("🆔 Our client ID is: %d", clientID)
        }
}

func (bot *ChatBot) getClientUsername(clientID int) (string, bool) {
        bot.clientMutex.RLock()
        defer bot.clientMutex.RUnlock()
        username, exists := bot.clientIDToUsername[clientID]
        return username, exists
}

func (bot *ChatBot) removeClient(clientID int) {
        bot.clientMutex.Lock()
        defer bot.clientMutex.Unlock()
        delete(bot.clientIDToUsername, clientID)
        
        bot.positionMutex.Lock()
        delete(bot.clientPositions, clientID)
        bot.positionMutex.Unlock()
}

// isAutopickEnabled safely checks if autopick is enabled
func (bot *ChatBot) isAutopickEnabled() bool {
        bot.autopickMutex.RLock()
        defer bot.autopickMutex.RUnlock()
        return bot.autopickEnabled
}

// setAutopickEnabled safely sets autopick status
func (bot *ChatBot) setAutopickEnabled(enabled bool) {
        bot.autopickMutex.Lock()
        bot.autopickEnabled = enabled
        bot.autopickMutex.Unlock()

        status := "DISABLED"
        if enabled {
                status = "ENABLED"
        }
        bot.log("🤖 Autopick %s", status)
        bot.updateFullStatus()
}

// updateClientPosition updates a client's position by ID
func (bot *ChatBot) updateClientPosition(clientID int, x, y int) {
        bot.positionMutex.Lock()
        defer bot.positionMutex.Unlock()
        
        oldPos, hadPosition := bot.clientPositions[clientID]
        bot.clientPositions[clientID] = Position{X: x, Y: y}
        
        if clientID == bot.ownClientID {
                bot.ownPosition = Position{X: x, Y: y}
                bot.log("📍 Own position updated: (%d, %d)", x, y)
        } else {
                // Check if this client has any items they dropped and moved away from
                if bot.isAutopickEnabled() && hadPosition && (oldPos.X != x || oldPos.Y != y) {
                        go bot.checkItemOpportunitiesForClient(clientID, oldPos)
                }
        }
}

// checkItemOpportunitiesForClient checks if client movement creates pickup opportunities
func (bot *ChatBot) checkItemOpportunitiesForClient(clientID int, oldPos Position) {
        // Check all tracked items to see if this client moved away from any
        bot.positionMutex.RLock()
        var itemsToCheck []DroppedItem
        for _, item := range bot.droppedItems {
                if item.DroppedByClientID == clientID && item.Position.X == oldPos.X && item.Position.Y == oldPos.Y {
                        itemsToCheck = append(itemsToCheck, item)
                }
        }
        bot.positionMutex.RUnlock()

        // Get username for logging
        username, _ := bot.getClientUsername(clientID)
        if username == "" {
                username = fmt.Sprintf("Client_%d", clientID)
        }

        // Immediately attempt to pick up items the client moved away from
        for _, item := range itemsToCheck {
                bot.log("⚡ %s (ID:%d) moved away from %s, immediate pickup attempt", username, clientID, item.Name)

                // Move to the item position immediately
                if err := bot.moveToPosition(item.Position.X, item.Position.Y); err != nil {
                        bot.log("❌ Failed to move to item position: %v", err)
                        continue
                }

                // Wait for movement command to be processed before pickup
                time.Sleep(500 * time.Millisecond)

                // Pick up the item immediately
                if err := bot.pickupItem(item.ID); err != nil {
                        bot.log("❌ Failed to pick up item: %v", err)
                        continue
                }

                // Remove from our tracked items
                bot.positionMutex.Lock()
                delete(bot.droppedItems, item.ID)
                bot.positionMutex.Unlock()

                bot.log("✅ Successfully picked up %s (ID:%d)", item.Name, item.ID)
                bot.tui.AddChatMessage("", "", fmt.Sprintf("🤖 Autopicked: %s", item.Name))

                // Only pick up one item at a time
                break
        }
}

// handleItemDropped processes when an item is dropped by a client
func (bot *ChatBot) handleItemDropped(itemID int, itemName string, x, y int, droppedByClientID int) {
        if !bot.isAutopickEnabled() {
                return
        }

        // Get username for this client ID
        username, _ := bot.getClientUsername(droppedByClientID)
        if username == "" {
                username = fmt.Sprintf("Client_%d", droppedByClientID)
        }

        bot.positionMutex.Lock()
        defer bot.positionMutex.Unlock()

        // Store the dropped item with client ID
        droppedItem := DroppedItem{
                ID:               itemID,
                Name:             itemName,
                Position:         Position{X: x, Y: y},
                DroppedByClientID: droppedByClientID,
                DroppedByUsername: username,
                DroppedAt:        time.Now(),
        }

        bot.droppedItems[itemID] = droppedItem
        bot.log("📦 Item dropped: %s (ID:%d) at (%d,%d) by %s (ClientID:%d)", itemName, itemID, x, y, username, droppedByClientID)

        // Start monitoring this item for pickup opportunity
        go bot.monitorDroppedItemByClientID(droppedItem)
}

// monitorDroppedItemByClientID watches a dropped item with client ID tracking
func (bot *ChatBot) monitorDroppedItemByClientID(item DroppedItem) {
        // Short initial wait to allow for position updates to propagate
        time.Sleep(100 * time.Millisecond)

        maxWaitTime := 5 * time.Second
        startTime := time.Now()
        checkInterval := 50 * time.Millisecond

        for time.Since(startTime) < maxWaitTime {
                // Check if item still exists
                bot.positionMutex.RLock()
                _, exists := bot.droppedItems[item.ID]
                bot.positionMutex.RUnlock()

                if !exists {
                        bot.log("📦 Item %s (ID:%d) no longer available", item.Name, item.ID)
                        return
                }

                // Check if the dropper is still at the item position
                bot.positionMutex.RLock()
                dropperPos, dropperExists := bot.clientPositions[item.DroppedByClientID]
                bot.positionMutex.RUnlock()

                // If dropper moved away or is no longer visible, attempt pickup
                if !dropperExists || (dropperPos.X != item.Position.X || dropperPos.Y != item.Position.Y) {
                        bot.log("🏃 %s (ClientID:%d) moved away from %s, attempting pickup", item.DroppedByUsername, item.DroppedByClientID, item.Name)

                        // Move to the item position
                        if err := bot.moveToPosition(item.Position.X, item.Position.Y); err != nil {
                                bot.log("❌ Failed to move to item position: %v", err)
                                return
                        }

                        // Wait for movement to be processed before pickup
                        time.Sleep(500 * time.Millisecond)

                        // Pick up the item
                        if err := bot.pickupItem(item.ID); err != nil {
                                bot.log("❌ Failed to pick up item: %v", err)
                                return
                        }

                        // Remove from tracking
                        bot.positionMutex.Lock()
                        delete(bot.droppedItems, item.ID)
                        bot.positionMutex.Unlock()

                        bot.log("✅ Successfully picked up %s (ID:%d)", item.Name, item.ID)
                        bot.tui.AddChatMessage("", "", fmt.Sprintf("🤖 Autopicked: %s", item.Name))
                        return
                }

                // Client still at item position, wait and check again
                time.Sleep(checkInterval)
        }

        // Timeout reached
        bot.log("⏱️ Timeout waiting for %s (ClientID:%d) to move away from %s", item.DroppedByUsername, item.DroppedByClientID, item.Name)
}

// handleItemPickedUp processes when an item is picked up (removes from tracking)
func (bot *ChatBot) handleItemPickedUp(itemID int) {
        bot.positionMutex.Lock()
        defer bot.positionMutex.Unlock()

        if item, exists := bot.droppedItems[itemID]; exists {
                delete(bot.droppedItems, itemID)
                bot.log("📦 Item %s (ID:%d) was picked up by someone", item.Name, itemID)
        }
}

// moveToPosition sends a move command to the specified coordinates
func (bot *ChatBot) moveToPosition(x, y int) error {
        if !bot.isConnectionHealthy() {
                return fmt.Errorf("WebSocket connection not healthy")
        }

        bot.wsMutex.Lock()
        defer bot.wsMutex.Unlock()

        if bot.wsConn == nil {
                return fmt.Errorf("WebSocket connection not established")
        }

        movePayload := map[string]interface{}{
                "type": "move",
                "data": map[string]interface{}{
                        "x": x,
                        "y": y,
                },
        }

        msgBytes, err := json.Marshal(movePayload)
        if err != nil {
                return fmt.Errorf("failed to marshal move command: %v", err)
        }

        bot.log("🚶 Moving to position (%d, %d)", x, y)
        bot.wsConn.SetWriteDeadline(time.Now().Add(10 * time.Second))
        err = bot.wsConn.WriteMessage(websocket.TextMessage, msgBytes)
        if err != nil {
                bot.setConnectionStatus(false)
                return fmt.Errorf("failed to send move command: %v", err)
        }

        return nil
}

// parseRoomFields parses and stores the room field data
func (bot *ChatBot) parseRoomFields(fieldsData []interface{}) {
        bot.fieldsMutex.Lock()
        defer bot.fieldsMutex.Unlock()

        bot.roomFields = make([]RoomField, 0)

        for _, fieldData := range fieldsData {
                if field, ok := fieldData.(map[string]interface{}); ok {
                        roomField := RoomField{}

                        if x, xOk := field["x"].(float64); xOk {
                                roomField.X = int(x)
                        }
                        if y, yOk := field["y"].(float64); yOk {
                                roomField.Y = int(y)
                        }
                        if state, stateOk := field["state"].(string); stateOk {
                                roomField.State = state
                        }
                        if isWalkable, walkableOk := field["isWalkable"].(bool); walkableOk {
                                roomField.IsWalkable = isWalkable
                        }

                        bot.roomFields = append(bot.roomFields, roomField)
                }
        }

        bot.log("🗺️ Parsed %d room fields", len(bot.roomFields))
}

// getAvailablePositions returns positions that are open, walkable, and not occupied
func (bot *ChatBot) getAvailablePositions() []Position {
        bot.fieldsMutex.RLock()
        defer bot.fieldsMutex.RUnlock()

        bot.positionMutex.RLock()
        defer bot.positionMutex.RUnlock()

        var available []Position

        // Get all occupied positions
        occupiedPositions := make(map[Position]bool)
        for _, clientPos := range bot.clientPositions {
                occupiedPositions[clientPos] = true
        }
        // Add our own position
        occupiedPositions[bot.ownPosition] = true

        // Find available fields
        for _, field := range bot.roomFields {
                if field.State == "open" && field.IsWalkable {
                        pos := Position{X: field.X, Y: field.Y}
                        if !occupiedPositions[pos] {
                                available = append(available, pos)
                        }
                }
        }

        return available
}

// moveToRandomPosition moves the bot to a random available position
func (bot *ChatBot) moveToRandomPosition() error {
        availablePositions := bot.getAvailablePositions()

        if len(availablePositions) == 0 {
                return fmt.Errorf("no available positions to move to")
        }

        // Select a random position
        randomIndex := time.Now().UnixNano() % int64(len(availablePositions))
        targetPos := availablePositions[randomIndex]

        bot.log("🎲 Moving to random position (%d, %d) out of %d available", targetPos.X, targetPos.Y, len(availablePositions))

        return bot.moveToPosition(targetPos.X, targetPos.Y)
}

// pickupItem sends a pickup command for the specified item ID
func (bot *ChatBot) pickupItem(itemID int) error {
        if !bot.isConnectionHealthy() {
                return fmt.Errorf("WebSocket connection not healthy")
        }

        bot.wsMutex.Lock()
        defer bot.wsMutex.Unlock()

        if bot.wsConn == nil {
                return fmt.Errorf("WebSocket connection not established")
        }

        pickupPayload := map[string]interface{}{
                "type": "pick up",
                "data": map[string]interface{}{
                        "item": itemID,
                },
        }

        msgBytes, err := json.Marshal(pickupPayload)
        if err != nil {
                return fmt.Errorf("failed to marshal pickup command: %v", err)
        }

        bot.log("📦 Picking up item ID: %d", itemID)
        bot.wsConn.SetWriteDeadline(time.Now().Add(10 * time.Second))
        err = bot.wsConn.WriteMessage(websocket.TextMessage, msgBytes)
        if err != nil {
                bot.setConnectionStatus(false)
                return fmt.Errorf("failed to send pickup command: %v", err)
        }

        return nil
}

// updateFullStatus updates the complete status bar
func (bot *ChatBot) updateFullStatus() {
        autopickStatus := "OFF"
        if bot.isAutopickEnabled() {
                autopickStatus = "ON"
        }
        bot.tui.UpdateStatusBar(bot.username, bot.isConnected, bot.currentRoomName, bot.currentRoom, autopickStatus)
}

// isConnectionHealthy checks if connection is healthy
func (bot *ChatBot) isConnectionHealthy() bool {
        bot.connMutex.RLock()
        defer bot.connMutex.RUnlock()
        return bot.isConnected && bot.wsConn != nil
}

// calculateReconnectDelay returns exponential backoff delay
func (bot *ChatBot) calculateReconnectDelay() time.Duration {
        baseDelay := 5 * time.Second
        maxDelay := 5 * time.Minute

        delay := time.Duration(baseDelay.Nanoseconds() * int64(1<<min(bot.reconnectCount, 6)))
        if delay > maxDelay {
                delay = maxDelay
        }

        return delay
}

// min helper function
func min(a, b int) int {
        if a < b {
                return a
        }
        return b
}

// fullReconnect performs complete reconnection including re-authentication
func (bot *ChatBot) fullReconnect() error {
        bot.reconnectCount++
        delay := bot.calculateReconnectDelay()

        bot.log("🔄 Attempting full reconnection #%d (delay: %v)...", bot.reconnectCount, delay)
        autopickStatus := "OFF"
        if bot.isAutopickEnabled() {
                autopickStatus = "ON"
        }
        bot.tui.UpdateStatusBar(bot.username, false, bot.currentRoomName, fmt.Sprintf("Reconnecting... (attempt %d)", bot.reconnectCount), autopickStatus)

        // Wait with exponential backoff
        time.Sleep(delay)

        // Close existing connection if any
        if bot.wsConn != nil {
                bot.wsConn.Close()
                bot.wsConn = nil
        }

        bot.setConnectionStatus(false)

        // Step 1: Re-authenticate if enough time has passed or if we've had many failures
        timeSinceLastReconnect := time.Since(bot.lastReconnect)
        if timeSinceLastReconnect > 10*time.Minute || bot.reconnectCount > 3 {
                bot.log("🔐 Re-authenticating due to extended disconnection...")
                if err := bot.login(); err != nil {
                        bot.log("❌ Re-authentication failed: %v", err)
                        return fmt.Errorf("re-authentication failed: %v", err)
                }
                bot.log("✅ Re-authentication successful")
        }

        // Step 2: Establish WebSocket connection
        bot.log("🔌 Re-establishing WebSocket connection...")
        if err := bot.connectWebSocket(); err != nil {
                bot.log("❌ WebSocket reconnection failed: %v", err)
                return fmt.Errorf("WebSocket reconnection failed: %v", err)
        }

        bot.setConnectionStatus(true)
        bot.lastReconnect = time.Now()
        bot.log("✅ Full reconnection successful!")
        bot.tui.AddChatMessage("", "", "*** Reconnected to chat server ***")

        return nil
}

// login handles the authentication process
func (bot *ChatBot) login() error {
        bot.log("🔐 Starting login process...")

        bot.log("📄 Getting login page: %s/chat", bot.baseURL)
        loginPageResp, err := bot.httpClient.Get(bot.baseURL + "/chat")
        if err != nil {
                return fmt.Errorf("failed to get login page: %v", err)
        }
        defer loginPageResp.Body.Close()

        bot.log("📄 Login page response status: %d", loginPageResp.StatusCode)
        bot.cookies = loginPageResp.Cookies()
        bot.log("🍪 Cookies after GET /chat: %d", len(bot.cookies))

        loginData := map[string]string{"username": bot.username, "password": bot.password}
        jsonData, err := json.Marshal(loginData)
        if err != nil {
                return fmt.Errorf("failed to marshal login data: %v", err)
        }

        req, err := http.NewRequest("POST", bot.baseURL+"/api/auth/login", strings.NewReader(string(jsonData)))
        if err != nil {
                return fmt.Errorf("failed to create login request: %v", err)
        }
        req.Header.Set("Content-Type", "application/json")
        req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
        req.Header.Set("Referer", bot.baseURL+"/chat")
        req.Header.Set("Accept", "application/json, text/plain, */*")

        for _, cookie := range bot.cookies {
                req.AddCookie(cookie)
        }

        bot.log("📤 Sending JSON POST request to /api/auth/login")
        resp, err := bot.httpClient.Do(req)
        if err != nil {
                return fmt.Errorf("login request failed: %v", err)
        }
        defer resp.Body.Close()

        respBody, _ := io.ReadAll(resp.Body)
        bot.log("📥 Login POST response status: %d", resp.StatusCode)
        bot.log("📥 Login POST response body: %s", string(respBody))

        currentResponseCookies := resp.Cookies()

        if resp.StatusCode == 200 || resp.StatusCode == 201 {
                var loginResponse map[string]interface{}
                if errJson := json.Unmarshal(respBody, &loginResponse); errJson == nil {
                        if id, ok := loginResponse["id"]; ok {
                                bot.log("✅ Login successful! User ID: %v, Username: %v", id, loginResponse["username"])
                                bot.cookies = currentResponseCookies
                                bot.log("🍪 Overwritten bot.cookies with %d cookies from successful login POST.", len(bot.cookies))
                                return nil
                        }
                        if success, ok := loginResponse["success"].(bool); ok && success {
                                bot.log("✅ Login successful via success field!")
                                bot.cookies = currentResponseCookies
                                bot.log("🍪 Overwritten bot.cookies with %d cookies from successful login POST (success field).", len(bot.cookies))
                                return nil
                        }
                }
                bot.log("✅ Login assumed successful based on status %d", resp.StatusCode)
                bot.cookies = currentResponseCookies
                bot.log("🍪 Overwritten bot.cookies with %d cookies from assumed successful login POST.", len(bot.cookies))
                return nil
        } else if resp.StatusCode == 302 {
                bot.log("✅ Login successful (redirect response %d)", resp.StatusCode)
                bot.cookies = currentResponseCookies
                bot.log("🍪 Overwritten bot.cookies with %d cookies from successful redirect login POST.", len(bot.cookies))
                return nil
        }

        return fmt.Errorf("login failed with status: %d, body: %s", resp.StatusCode, string(respBody))
}

// connectWebSocket establishes the WebSocket connection
func (bot *ChatBot) connectWebSocket() error {
        bot.log("🔌 Starting WebSocket connection...")
        headers := http.Header{}
        headers.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
        headers.Set("Origin", bot.baseURL)

        finalCookiesForHeaderMap := make(map[string]string)
        for _, cookie := range bot.cookies {
                finalCookiesForHeaderMap[cookie.Name] = cookie.Value
        }

        if len(finalCookiesForHeaderMap) > 0 {
                var cookieStrings []string
                for name, value := range finalCookiesForHeaderMap {
                        cookieStrings = append(cookieStrings, fmt.Sprintf("%s=%s", name, value))
                }
                headers.Set("Cookie", strings.Join(cookieStrings, "; "))
                bot.log("🍪 WebSocket Cookie header (uniqued names by map): %s", headers.Get("Cookie"))
        } else {
                bot.log("⚠️ No cookies to send for WebSocket header")
        }

        var latestSessionCookieForURLFallback *http.Cookie
        for i := len(bot.cookies) - 1; i >= 0; i-- {
                cookie := bot.cookies[i]
                if cookie.Name == "session" || cookie.Name == "connect.sid" || cookie.Name == "sessionid" || cookie.Name == "PHPSESSID" {
                        latestSessionCookieForURLFallback = cookie
                        valPreview := cookie.Value
                        if len(valPreview) > 10 {
                                valPreview = valPreview[:10] + "..."
                        }
                        bot.log("🍪 Identified latest session cookie for potential URL fallback: %s (value: %s)", cookie.Name, valPreview)
                        break
                }
        }

        bot.log("🔌 Connecting to: %s", bot.wsURL)
        dialer := websocket.Dialer{HandshakeTimeout: 45 * time.Second}
        conn, resp, err := dialer.Dial(bot.wsURL, headers)

        if err != nil {
                errMsg := fmt.Sprintf("failed to connect to WebSocket (initial attempt): %v", err)
                if resp != nil {
                        errMsg += fmt.Sprintf(", status: %d", resp.StatusCode)
                        bodyBytes, _ := io.ReadAll(resp.Body)
                        errMsg += fmt.Sprintf(", response: %s", string(bodyBytes))
                        resp.Body.Close()
                }
                bot.log("❌ %s", errMsg)

                if latestSessionCookieForURLFallback != nil {
                        wsURLWithToken := fmt.Sprintf("%s?session_token=%s", bot.wsURL, latestSessionCookieForURLFallback.Value)
                        bot.log("🔄 Trying WebSocket connection with LATEST session token in URL")
                        conn, resp, err = dialer.Dial(wsURLWithToken, headers)
                        if err == nil {
                                bot.log("✅ WebSocket connected with session token in URL!")
                        } else {
                                errMsgToken := fmt.Sprintf("failed to connect with token in URL: %v", err)
                                if resp != nil {
                                        errMsgToken += fmt.Sprintf(", status: %d", resp.StatusCode)
                                        bodyBytes, _ := io.ReadAll(resp.Body)
                                        errMsgToken += fmt.Sprintf(", response: %s", string(bodyBytes))
                                        resp.Body.Close()
                                }
                                bot.log("❌ %s", errMsgToken)
                                return fmt.Errorf(errMsgToken)
                        }
                } else {
                        bot.log("⚠️ No session cookie identified to attempt URL token fallback.")
                        return fmt.Errorf(errMsg)
                }
        }

        if resp != nil {
                bot.log("✅ WebSocket handshake successful! Status: %d", resp.StatusCode)
                resp.Body.Close()
        }
        bot.wsConn = conn
        bot.log("✅ WebSocket connected successfully!")
        return nil
}

// sendMessageInternal sends a message over WebSocket and optionally resets the periodic timer
func (bot *ChatBot) sendMessageInternal(message string, resetTimer bool) error {
        if !bot.isConnectionHealthy() {
                return fmt.Errorf("WebSocket connection not healthy")
        }

        bot.wsMutex.Lock()
        defer bot.wsMutex.Unlock()

        if bot.wsConn == nil {
                return fmt.Errorf("WebSocket connection not established")
        }

        bot.log("💬 Preparing to send message to room %s: '%s'", bot.currentRoom, message)

        msgPayload := map[string]interface{}{
                "type": "chat",
                "data": map[string]interface{}{
                        "message": message,
                        "room":    bot.currentRoom,
                },
        }

        msgBytes, err := json.Marshal(msgPayload)
        if err != nil {
                bot.log("❌ Failed to marshal message: %v. Payload: %+v", err, msgPayload)
                return fmt.Errorf("failed to marshal message: %v", err)
        }

        bot.log("📤 Sending message: %s", string(msgBytes))

        // Set a write deadline to prevent hanging
        bot.wsConn.SetWriteDeadline(time.Now().Add(10 * time.Second))
        err = bot.wsConn.WriteMessage(websocket.TextMessage, msgBytes)
        if err != nil {
                bot.log("❌ Failed to send message: %v", err)
                // Mark connection as unhealthy on write failure
                bot.setConnectionStatus(false)
                return fmt.Errorf("failed to send message: %v", err)
        }

        if resetTimer {
                bot.resetMessageTimer()
                bot.log("⏰ Periodic message timer reset after sending manual message.")
        }

        bot.log("✅ Message sent successfully: '%s'", message)
        return nil
}

// sendMessage is a convenience wrapper for sending periodic messages
func (bot *ChatBot) sendMessage(message string) error {
        return bot.sendMessageInternal(message, false)
}

// sendConsoleMessage is for messages typed by user
func (bot *ChatBot) sendConsoleMessage(message string) error {
        err := bot.sendMessageInternal(message, true)
        if err == nil {
                // Show the message in chat pane as well (in goroutine to avoid blocking)
                go func() {
                        bot.tui.AddChatMessage("", bot.username, message)
                }()
        }
        return err
}

// resetMessageTimer stops and resets the periodic message timer
func (bot *ChatBot) resetMessageTimer() {
        bot.timerMutex.Lock()
        defer bot.timerMutex.Unlock()

        if bot.messageTimer != nil {
                if !bot.messageTimer.Stop() {
                        select {
                        case <-bot.messageTimer.C:
                        default:
                        }
                }
                bot.messageTimer.Reset(bot.messageInterval)
                bot.log("⏰ Message timer reset to %v", bot.messageInterval)
        } else {
                bot.log("⚠️ Tried to reset a nil message timer.")
        }
}

// sendAcknowledgment sends a specific acknowledgment message to the server
func (bot *ChatBot) sendAcknowledgment(messageType string) error {
        if !bot.isConnectionHealthy() {
                return fmt.Errorf("WebSocket connection not healthy")
        }

        bot.wsMutex.Lock()
        defer bot.wsMutex.Unlock()

        if bot.wsConn == nil {
                return fmt.Errorf("WebSocket connection not established")
        }

        bot.log("ℹ️ Preparing to send acknowledgment for server event: '%s'", messageType)

        var ackPayload interface{}

        switch messageType {
        case "newHour":
                ackPayload = map[string]interface{}{
                        "type": "hour",
                }
                bot.log("✅ Using specific acknowledgment payload for '%s'", messageType)
        default:
                bot.log("⚠️ No specific acknowledgment logic defined for messageType: '%s'", messageType)
                return fmt.Errorf("no specific acknowledgment handler defined for messageType: %s", messageType)
        }

        msgBytes, err := json.Marshal(ackPayload)
        if err != nil {
                bot.log("❌ Failed to marshal acknowledgment payload for '%s': %v", messageType, err)
                return fmt.Errorf("failed to marshal acknowledgment payload for '%s': %v", messageType, err)
        }

        bot.log("📤 Sending acknowledgment for '%s': %s", messageType, string(msgBytes))
        bot.wsConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
        err = bot.wsConn.WriteMessage(websocket.TextMessage, msgBytes)
        if err != nil {
                bot.log("❌ Failed to send acknowledgment for '%s': %v", messageType, err)
                // Mark connection as unhealthy on write failure
                bot.setConnectionStatus(false)
                return fmt.Errorf("failed to send acknowledgment for '%s': %v", messageType, err)
        }

        bot.log("✅ Acknowledgment for '%s' sent successfully.", messageType)
        return nil
}

// listenForMessages reads messages from WebSocket and processes them
func (bot *ChatBot) listenForMessages() {
        bot.log("👂 Starting message listener...")

        for {
                // Check if we need to reconnect
                if !bot.isConnectionHealthy() {
                        bot.log("🔄 Connection unhealthy, attempting full reconnection...")

                        if err := bot.fullReconnect(); err != nil {
                                bot.log("❌ Full reconnection failed: %v. Will retry...", err)
                                continue
                        }

                        bot.log("✅ Reconnection successful, resuming message listening...")
                        continue
                }

                // Set read deadline to detect dead connections
                bot.wsConn.SetReadDeadline(time.Now().Add(90 * time.Second))
                _, message, err := bot.wsConn.ReadMessage()

                if err != nil {
                        bot.log("❌ Error reading message: %v", err)

                        // Mark connection as unhealthy
                        bot.setConnectionStatus(false)

                        // Close the connection
                        if bot.wsConn != nil {
                                bot.wsConn.Close()
                                bot.wsConn = nil
                        }

                        bot.log("🔄 Connection lost, will attempt reconnection...")
                        continue
                }

                // Reset read deadline on successful read
                bot.wsConn.SetReadDeadline(time.Time{})
                bot.log("📥 Raw message received: %s", string(message))

                var messageData map[string]interface{}
                if errJson := json.Unmarshal(message, &messageData); errJson == nil {
                        // Handle room changes and initial room data
                        if roomInfo, ok := messageData["room"].(map[string]interface{}); ok {
                                if idVal, idOk := roomInfo["id"]; idOk {
                                        newRoomID := fmt.Sprintf("%v", idVal)
                                        if nameVal, nameOk := roomInfo["name"].(string); nameOk {
                                                if bot.currentRoom != newRoomID {
                                                        bot.log("🚪 Room changed by server: %s -> %s (%s)", bot.currentRoom, newRoomID, nameVal)
                                                        bot.currentRoom = newRoomID
                                                        bot.currentRoomName = nameVal
                                                        bot.tui.SetRoom(newRoomID, nameVal)
                                                        bot.updateFullStatus()
                                                }
                                        }
                                }

                                // Parse room fields for movement
                                if fields, fieldsOk := roomInfo["fields"].([]interface{}); fieldsOk {
                                        bot.parseRoomFields(fields)
                                }

                                // Parse initial client list when joining room
                                if clientsData, clientsOk := messageData["clients"].(map[string]interface{}); clientsOk {
                                        if updates, updatesOk := clientsData["updates"].([]interface{}); updatesOk {
                                                var users []string
                                                for _, update := range updates {
                                                        if client, clientOk := update.(map[string]interface{}); clientOk {
                                                                var clientID int
                                                                var username string

                                                                if id, idOk := client["id"].(float64); idOk {
                                                                        clientID = int(id)
                                                                }
                                                                if name, nameOk := client["username"].(string); nameOk {
                                                                        username = name
                                                                        users = append(users, username)
                                                                        
                                                                        // Store client ID mapping
                                                                        if clientID != 0 {
                                                                                bot.setClientUsername(clientID, username)
                                                                        }

                                                                        // Track initial positions
                                                                        if x, xOk := client["x"].(float64); xOk {
                                                                                if y, yOk := client["y"].(float64); yOk {
                                                                                        bot.updateClientPosition(clientID, int(x), int(y))
                                                                                }
                                                                        }
                                                                }
                                                        }
                                                }
                                                bot.tui.SetRoomUsers(users)
                                                bot.log("👥 Found %d users in room", len(users))
                                        }
                                }
                        }

                        // Handle client updates (user joins and position updates)
                        if clientsData, ok := messageData["clients"].(map[string]interface{}); ok {
                                // Handle user updates
                                if updates, updatesOk := clientsData["updates"].([]interface{}); updatesOk {
                                        for _, update := range updates {
                                                if client, clientOk := update.(map[string]interface{}); clientOk {
                                                        var clientID int
                                                        var username string
                                                        var userX, userY int
                                                        hasPosition := false

                                                        // Extract client ID
                                                        if id, idOk := client["id"].(float64); idOk {
                                                                clientID = int(id)
                                                        }

                                                        // Extract username if present
                                                        if name, nameOk := client["username"].(string); nameOk {
                                                                username = name
                                                                // Store the client ID to username mapping
                                                                if clientID != 0 {
                                                                        bot.setClientUsername(clientID, username)
                                                                }
                                                        }

                                                        // Extract position if present
                                                        if x, xOk := client["x"].(float64); xOk {
                                                                userX = int(x)
                                                                hasPosition = true
                                                                if y, yOk := client["y"].(float64); yOk {
                                                                        userY = int(y)
                                                                }
                                                        }

                                                        // Update position tracking by client ID
                                                        if clientID != 0 && hasPosition {
                                                                bot.updateClientPosition(clientID, userX, userY)
                                                        }

                                                        // Handle new user joins (when we have username but no room data)
                                                        if username != "" && clientID != 0 {
                                                                if _, hasRoom := messageData["room"]; !hasRoom {
                                                                        bot.tui.AddUser(username)
                                                                        bot.log("👤 User joined: %s (ClientID:%d)", username, clientID)
                                                                }
                                                        }

                                                        // Handle chat messages
                                                        if events, eventsOk := client["events"].([]interface{}); eventsOk {
                                                                for _, event := range events {
                                                                        if eventData, eventOk := event.(map[string]interface{}); eventOk {
                                                                                if eventType, typeOk := eventData["type"].(string); typeOk && eventType == "chat" {
                                                                                        if data, dataOk := eventData["data"].(map[string]interface{}); dataOk {
                                                                                                if chatUsername, usernameOk := data["username"].(string); usernameOk {
                                                                                                        if message, messageOk := data["message"].(string); messageOk {
                                                                                                                bot.log("💬 Chat [%s]: %s", chatUsername, message)
                                                                                                                if chatUsername != bot.username {
                                                                                                                        bot.tui.AddChatMessage("", chatUsername, message)
                                                                                                                }
                                                                                                        }
                                                                                                }
                                                                                        }
                                                                                }
                                                                        }
                                                                }
                                                        }

                                                        // Handle item removal (items being dropped) - now using client ID
                                                        if items, itemsOk := client["items"].(map[string]interface{}); itemsOk {
                                                                if removes, removesOk := items["removes"].([]interface{}); removesOk {
                                                                        for _, removeItem := range removes {
                                                                                if itemID, itemIDOk := removeItem.(float64); itemIDOk {
                                                                                        // Check if there's a corresponding item update (item being dropped)
                                                                                        if itemsUpdates, hasItemsUpdates := messageData["items"].(map[string]interface{}); hasItemsUpdates {
                                                                                                if updatesArray, updatesOk := itemsUpdates["updates"].([]interface{}); updatesOk {
                                                                                                        for _, itemUpdate := range updatesArray {
                                                                                                                if item, itemOk := itemUpdate.(map[string]interface{}); itemOk {
                                                                                                                        if id, idOk := item["id"].(float64); idOk && int(id) == int(itemID) {
                                                                                                                                // This is an item being dropped
                                                                                                                                itemName := "Unknown Item"
                                                                                                                                if name, nameOk := item["name"].(string); nameOk {
                                                                                                                                        itemName = name
                                                                                                                                }
                                                                                                                                itemX, itemY := 0, 0
                                                                                                                                if x, xOk := item["x"].(float64); xOk {
                                                                                                                                        itemX = int(x)
                                                                                                                                }
                                                                                                                                if y, yOk := item["y"].(float64); yOk {
                                                                                                                                        itemY = int(y)
                                                                                                                                }

                                                                                                                                bot.handleItemDropped(int(itemID), itemName, itemX, itemY, clientID)
                                                                                                                        }
                                                                                                                }
                                                                                                        }
                                                                                                }
                                                                                        }
                                                                                }
                                                                        }
                                                                }
                                                        }
                                                }
                                        }
                                }

                                // Handle user leaves/disconnects (removes)
                                if removes, removesOk := clientsData["removes"].([]interface{}); removesOk {
                                        for _, removeData := range removes {
                                                if clientID, clientIDOk := removeData.(float64); clientIDOk {
                                                        // Remove from client mapping
                                                        bot.removeClient(int(clientID))
                                                }
                                        }
                                        
                                        // We need to track user IDs to usernames to handle removes properly
                                        // For now, we'll parse the system message to get the username
                                        if player, playerOk := messageData["player"].(map[string]interface{}); playerOk {
                                                if events, eventsOk := player["events"].([]interface{}); eventsOk {
                                                        for _, event := range events {
                                                                if eventData, eventOk := event.(map[string]interface{}); eventOk {
                                                                        if eventType, typeOk := eventData["type"].(string); typeOk && eventType == "system" {
                                                                                if data, dataOk := eventData["data"].(map[string]interface{}); dataOk {
                                                                                        if message, msgOk := data["message"].(string); msgOk {
                                                                                                // Parse system messages like "XXX gik til Receptionen" or "XXX mistede forbindelsen"
                                                                                                if strings.Contains(message, "gik til") || strings.Contains(message, "mistede forbindelsen") {
                                                                                                        parts := strings.Fields(message)
                                                                                                        if len(parts) > 0 {
                                                                                                                username := parts[0]
                                                                                                                bot.tui.RemoveUser(username)
                                                                                                                bot.tui.AddChatMessage("", "", message)
                                                                                                                bot.log("👤 User left/disconnected: %s", username)
                                                                                                        }
                                                                                                }
                                                                                        }
                                                                                }
                                                                        }
                                                                }
                                                        }
                                                }
                                        }
                                }
                        }

                        // Handle item pickups (items being removed from the game)
                        if itemsData, ok := messageData["items"].(map[string]interface{}); ok {
                                if removes, removesOk := itemsData["removes"].([]interface{}); removesOk {
                                        for _, removeItem := range removes {
                                                if itemID, itemIDOk := removeItem.(float64); itemIDOk {
                                                        // This is an item being picked up by someone
                                                        bot.handleItemPickedUp(int(itemID))
                                                }
                                        }
                                }
                        }

                        // Handle system events
                        if player, hasPlayer := messageData["player"].(map[string]interface{}); hasPlayer {
                                // Handle onlineTime updates
                                if onlineTime, hasOnlineTime := player["onlineTime"].(map[string]interface{}); hasOnlineTime {
                                        if hours, hasHours := onlineTime["hours"]; hasHours {
                                                if minutes, hasMinutes := onlineTime["minutes"]; hasMinutes {
                                                        if h, hOk := hours.(float64); hOk {
                                                                if m, mOk := minutes.(float64); mOk {
                                                                        bot.tui.UpdateOnlineTime(int(h), int(m))
                                                                        bot.updateFullStatus()
                                                                        bot.log("⏰ Online time updated: %dh %dm", int(h), int(m))
                                                                }
                                                        }
                                                }
                                        }
                                }

                                // Handle newHour events
                                if newHour, hasNewHour := player["newHour"].(bool); hasNewHour && newHour {
                                        bot.log("🕐 'newHour' event detected from server - sending acknowledgment...")
                                        go func() {
                                                if errAck := bot.sendAcknowledgment("newHour"); errAck != nil {
                                                        bot.log("❌ Failed to acknowledge 'newHour' event: %v", errAck)
                                                }
                                        }()
                                }

                                // Handle system events like user arrivals
                                if events, eventsOk := player["events"].([]interface{}); eventsOk {
                                        for _, event := range events {
                                                if eventData, eventOk := event.(map[string]interface{}); eventOk {
                                                        if eventType, typeOk := eventData["type"].(string); typeOk && eventType == "system" {
                                                                if data, dataOk := eventData["data"].(map[string]interface{}); dataOk {
                                                                        if message, msgOk := data["message"].(string); msgOk {
                                                                                // Handle user arrival messages
                                                                                if strings.Contains(message, "ankom til rummet") {
                                                                                        parts := strings.Fields(message)
                                                                                        if len(parts) > 0 {
                                                                                                username := parts[0]
                                                                                                bot.tui.AddUser(username)
                                                                                                bot.tui.AddChatMessage("", "", message)
                                                                                                bot.log("👤 User arrived: %s", username)
                                                                                        }
                                                                                } else if strings.Contains(message, "Du er ankommet til") {
                                                                                        // Handle own arrival message
                                                                                        bot.tui.AddChatMessage("", "", message)
                                                                                }
                                                                        }
                                                                }
                                                        }
                                                }
                                        }
                                }
                        }

                        // Handle chat messages - display in chat pane (legacy format, keeping for compatibility)
                        if msgType, ok := messageData["type"].(string); ok && (msgType == "chat" || msgType == "message") {
                                if data, dataOk := messageData["data"].(map[string]interface{}); dataOk {
                                        username, _ := data["username"].(string)
                                        chatMsg, _ := data["message"].(string)
                                        if username != "" && chatMsg != "" {
                                                bot.log("💬 Chat [%s]: %s", username, chatMsg)
                                                // Don't display your own messages again (they're already shown when sent)
                                                if username != bot.username {
                                                        bot.tui.AddChatMessage("", username, chatMsg)
                                                }
                                        }
                                }
                        }
                        if user, userOk := messageData["user"].(string); userOk {
                                if text, textOk := messageData["text"].(string); textOk {
                                        bot.log("💬 Chat [%s]: %s", user, text)
                                        // Don't display your own messages again (they're already shown when sent)
                                        if user != bot.username {
                                                bot.tui.AddChatMessage("", user, text)
                                        }
                                }
                        }
                } else {
                        bot.log("⚠️ Could not parse message as JSON: %v. Message: %s", errJson, string(message))
                }
        }
}

// sendPeriodicMessages sends messages from the configured list at intervals
func (bot *ChatBot) sendPeriodicMessages() {
        if len(bot.messages) == 0 || bot.messages[0] == "ADD_YOUR_MESSAGES_HERE" {
                bot.log("⚠️ No valid periodic messages configured. Periodic sender will not run.")
                return
        }
        bot.log("🕐 Starting periodic message sender (interval: %v)", bot.messageInterval)

        bot.timerMutex.Lock()
        if bot.messageTimer == nil {
                bot.messageTimer = time.NewTimer(bot.messageInterval)
        } else {
                bot.messageTimer.Reset(bot.messageInterval)
        }
        bot.timerMutex.Unlock()

        for {
                bot.timerMutex.Lock()
                timerC := bot.messageTimer.C
                bot.timerMutex.Unlock()

                <-timerC

                // Only send if connection is healthy
                if bot.isConnectionHealthy() && len(bot.messages) > 0 {
                        message := bot.messages[bot.currentMsgIndex]
                        bot.log("⏰ Time to send periodic message [%d/%d]: '%s'", bot.currentMsgIndex+1, len(bot.messages), message)
                        if err := bot.sendMessage(message); err != nil {
                                bot.log("❌ Failed to send periodic message: %v", err)
                                // Don't advance message index on failure
                        } else {
                                bot.log("✅ Periodic message sent successfully: '%s'", message)
                                // Show the message in chat as well
                                bot.tui.AddChatMessage("", bot.username, message)
                                bot.currentMsgIndex = (bot.currentMsgIndex + 1) % len(bot.messages)
                        }
                } else if !bot.isConnectionHealthy() {
                        bot.log("⚠️ Skipping periodic message - connection not healthy")
                }

                bot.timerMutex.Lock()
                if bot.messageTimer != nil {
                        bot.messageTimer.Reset(bot.messageInterval)
                }
                bot.timerMutex.Unlock()
        }
}

// setupInputHandler configures the input field for commands and messages
func (bot *ChatBot) setupInputHandler() {
        bot.tui.inputField.SetDoneFunc(func(key tcell.Key) {
                if key == tcell.KeyEnter {
                        input := strings.TrimSpace(bot.tui.inputField.GetText())
                        if input == "" {
                                return
                        }

                        // Clear input field immediately for better UX
                        bot.tui.inputField.SetText("")

                        // Handle commands and messages in goroutine to not block TUI
                        go func(message string) {
                                // Handle commands
                                if strings.HasPrefix(message, "/") {
                                        command := strings.Fields(message)[0]

                                        switch command {
                                        case "/quit", "/exit", "/q":
                                                bot.log("⌨️ Quit command received. Shutting down...")
                                                bot.Stop()
                                                bot.tui.app.Stop()
                                                os.Exit(0)
                                        case "/help", "/h":
                                                bot.tui.AddChatMessage("", "", "Available Commands:")
                                                bot.tui.AddChatMessage("", "", "/quit, /exit, /q - Exit the bot")
                                                bot.tui.AddChatMessage("", "", "/help, /h - Show this help")
                                                bot.tui.AddChatMessage("", "", "/status - Show bot status")
                                                bot.tui.AddChatMessage("", "", "/reconnect - Force reconnection")
                                                bot.tui.AddChatMessage("", "", "/clear - Clear chat history")
                                                bot.tui.AddChatMessage("", "", "/logs - Show log file information")
                                                bot.tui.AddChatMessage("", "", "/autopick - Toggle autopick on/off")
                                                bot.tui.AddChatMessage("", "", "/move - Move to random open field")
                                        case "/status":
                                                wsStatus := "Disconnected"
                                                if bot.isConnectionHealthy() {
                                                        wsStatus = "Connected"
                                                }
                                                autopickStatus := "DISABLED"
                                                if bot.isAutopickEnabled() {
                                                        autopickStatus = "ENABLED"
                                                }
                                                hours, minutes := bot.tui.GetOnlineTime()
                                                bot.tui.AddChatMessage("", "", fmt.Sprintf("Username: %s", bot.username))
                                                bot.tui.AddChatMessage("", "", fmt.Sprintf("Current Room: %s (%s)", bot.currentRoomName, bot.currentRoom))
                                                bot.tui.AddChatMessage("", "", fmt.Sprintf("WebSocket: %s", wsStatus))
                                                bot.tui.AddChatMessage("", "", fmt.Sprintf("Online Time: %dh %dm", hours, minutes))
                                                bot.tui.AddChatMessage("", "", fmt.Sprintf("Message Interval: %v", bot.messageInterval))
                                                bot.tui.AddChatMessage("", "", fmt.Sprintf("Autopick: %s", autopickStatus))
                                                bot.tui.AddChatMessage("", "", fmt.Sprintf("Reconnect Count: %d", bot.reconnectCount))
                                                if !bot.lastReconnect.IsZero() {
                                                        bot.tui.AddChatMessage("", "", fmt.Sprintf("Last Reconnect: %s", bot.lastReconnect.Format("15:04:05")))
                                                }

                                                // Show tracked items if autopick is enabled
                                                if bot.isAutopickEnabled() {
                                                        bot.positionMutex.RLock()
                                                        itemCount := len(bot.droppedItems)
                                                        userCount := len(bot.clientPositions)
                                                        bot.positionMutex.RUnlock()
                                                        bot.tui.AddChatMessage("", "", fmt.Sprintf("Tracked Items: %d", itemCount))
                                                        bot.tui.AddChatMessage("", "", fmt.Sprintf("Tracked Clients: %d", userCount))
                                                }
                                        case "/reconnect":
                                                bot.tui.AddChatMessage("", "", "*** Forcing reconnection... ***")
                                                go func() {
                                                        bot.setConnectionStatus(false)
                                                        if bot.wsConn != nil {
                                                                bot.wsConn.Close()
                                                                bot.wsConn = nil
                                                        }
                                                        if err := bot.fullReconnect(); err != nil {
                                                                bot.log("❌ Manual reconnection failed: %v", err)
                                                                bot.tui.AddChatMessage("", "", "*** Reconnection failed ***")
                                                        }
                                                }()
                                        case "/move":
                                                availablePositions := bot.getAvailablePositions()
                                                if len(availablePositions) == 0 {
                                                        bot.tui.AddChatMessage("", "", "❌ No available positions to move to")
                                                } else {
                                                        if err := bot.moveToRandomPosition(); err != nil {
                                                                bot.tui.AddChatMessage("", "", fmt.Sprintf("❌ Failed to move: %v", err))
                                                        } else {
                                                                bot.tui.AddChatMessage("", "", fmt.Sprintf("🎲 Moving to random position (%d available)", len(availablePositions)))
                                                        }
                                                }
                                        case "/autopick":
                                                currentStatus := bot.isAutopickEnabled()
                                                bot.setAutopickEnabled(!currentStatus)
                                                newStatus := "ENABLED"
                                                if !bot.isAutopickEnabled() {
                                                        newStatus = "DISABLED"
                                                }
                                                bot.tui.AddChatMessage("", "", fmt.Sprintf("🤖 Autopick %s", newStatus))
                                        case "/logs":
                                                currentDate := time.Now().Format("2006-01-02")
                                                chatLogPath := fmt.Sprintf("logs/%s_chat_log.txt", currentDate)
                                                rawLogPath := fmt.Sprintf("logs/%s_raw_log.txt", currentDate)

                                                bot.tui.AddChatMessage("", "", "📁 Log File Information:")
                                                bot.tui.AddChatMessage("", "", fmt.Sprintf("Chat Log: %s", chatLogPath))
                                                bot.tui.AddChatMessage("", "", fmt.Sprintf("System Log: %s", rawLogPath))
                                                bot.tui.AddChatMessage("", "", "All logs are saved with daily rotation")
                                                bot.tui.AddChatMessage("", "", "Format: YYYY-MM-DD_[chat_log|raw_log].txt")
                                        case "/clear":
                                                bot.tui.app.QueueUpdateDraw(func() {
                                                        bot.tui.chatView.Clear()
                                                })
                                        default:
                                                // Send command as message
                                                bot.log("⌨️ Sending command as message: '%s'", message)
                                                if err := bot.sendConsoleMessage(message); err != nil {
                                                        bot.log("❌ Failed to send console message: %v", err)
                                                }
                                        }
                                } else {
                                        // Send regular message
                                        bot.log("⌨️ Sending message: '%s'", message)
                                        if err := bot.sendConsoleMessage(message); err != nil {
                                                bot.log("❌ Failed to send console message: %v", err)
                                        }
                                }
                        }(input)
                }
        })

        // Set input field as focusable and ensure it gets focus
        bot.tui.app.SetFocus(bot.tui.inputField)
}

// sendKeepAlive sends WebSocket ping messages periodically
func (bot *ChatBot) sendKeepAlive() {
        bot.log("💓 Starting keep-alive sender (30 second interval)")
        ticker := time.NewTicker(30 * time.Second)
        defer ticker.Stop()

        for range ticker.C {
                if bot.isConnectionHealthy() {
                        bot.wsMutex.Lock()
                        bot.wsConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
                        if err := bot.wsConn.WriteMessage(websocket.PingMessage, []byte("keepalive")); err != nil {
                                bot.log("❌ Failed to send ping: %v", err)
                                // Mark connection as unhealthy on ping failure
                                bot.setConnectionStatus(false)
                        } else {
                                bot.log("💓 Keep-alive ping sent successfully")
                        }
                        bot.wsMutex.Unlock()
                } else {
                        bot.log("⚠️ Skipping keep-alive ping - connection not healthy")
                }
        }
}

// startStatusUpdater keeps the status bar current time updated
func (bot *ChatBot) startStatusUpdater() {
        bot.log("⏰ Starting status updater (5 second interval)")
        ticker := time.NewTicker(5 * time.Second)
        defer ticker.Stop()

        for range ticker.C {
                // Update the status bar with current time (other info stays the same)
                bot.updateFullStatus()
        }
}

// Start initializes and runs the bot
func (bot *ChatBot) Start() error {
        bot.log("🚀 Starting Skyskraber chat bot...")
        bot.tui.UpdateStatusBar(bot.username, false, "", "Starting...", "OFF")

        bot.log("🔐 Step 1: Authenticating...")
        bot.tui.UpdateStatusBar(bot.username, false, "", "Authenticating...", "OFF")
        if err := bot.login(); err != nil {
                return fmt.Errorf("login failed: %v", err)
        }

        bot.log("🔌 Step 2: Establishing WebSocket connection...")
        bot.tui.UpdateStatusBar(bot.username, false, "", "Connecting...", "OFF")
        if err := bot.connectWebSocket(); err != nil {
                return fmt.Errorf("WebSocket connection failed: %v", err)
        }

        // Mark as connected after successful connection
        bot.setConnectionStatus(true)
        bot.lastReconnect = time.Now()

        bot.log("👂 Step 3: Starting message listener...")
        go bot.listenForMessages()

        bot.log("🕐 Step 4: Starting periodic message sender...")
        go bot.sendPeriodicMessages()

        bot.log("💓 Step 5: Starting keep-alive routine...")
        go bot.sendKeepAlive()

        bot.log("⏰ Step 6: Starting status updater...")
        go bot.startStatusUpdater()

        bot.log("⌨️ Step 7: Setting up input handler...")
        bot.setupInputHandler()

        bot.log("✅ Bot started successfully! 🎉")
        bot.updateFullStatus()

        bot.tui.AddChatMessage("", "", "=== Skyskraber Chat Bot Started ===")
        bot.tui.AddChatMessage("", "", "Type /help for available commands")
        bot.tui.AddChatMessage("", "", "Press Ctrl+C or type /quit to exit")
        bot.tui.AddChatMessage("", "", "📁 Logs are being saved to logs/ directory")
        bot.tui.AddChatMessage("", "", "🤖 Autopick is DISABLED by default - use /autopick to enable")
        bot.tui.AddChatMessage("", "", "🎲 Use /move to move to random open field")
        bot.tui.AddChatMessage("", "", "⬆️⬇️ Use arrow keys to navigate input history")

        return nil
}

// Stop gracefully shuts down the bot
func (bot *ChatBot) Stop() {
        bot.log("🛑 Stopping bot...")

        bot.timerMutex.Lock()
        if bot.messageTimer != nil {
                bot.messageTimer.Stop()
                bot.log("⏰ Periodic message timer stopped.")
        }
        bot.timerMutex.Unlock()

        if bot.wsConn != nil {
                bot.log("🔌 Closing WebSocket connection...")
                err := bot.wsConn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
                if err != nil {
                        bot.log("⚠️ Error sending close message: %v", err)
                }
                bot.wsConn.Close()
                bot.wsConn = nil
                bot.log("🔌 WebSocket connection closed.")
        }

        bot.log("📁 Closing log files...")
        bot.tui.closeLogFiles()
        bot.log("✅ Bot stopped.")
}

// main is the entry point of the application
func main() {
        config, err := loadConfig("config.json")
        if err != nil {
                fmt.Printf("❌ CRITICAL: Failed to load config: %v\n", err)
                os.Exit(1)
        }

        if strings.Contains(config.Username, "CHANGE_THIS") ||
                strings.Contains(config.Password, "CHANGE_THIS") ||
                (len(config.Messages) > 0 && strings.Contains(config.Messages[0], "ADD_YOUR_MESSAGES")) {
                fmt.Println("🚨 CONFIGURATION REQUIRED:")
                fmt.Println("Please edit 'config.json' and update:")
                fmt.Println("- username: your actual username")
                fmt.Println("- password: your actual password")
                fmt.Println("- messages: your custom messages array")
                fmt.Println("Then run the bot again!")
                os.Exit(1)
        }

        // Handle command line arguments
        if len(os.Args) > 1 {
                config.Username = os.Args[1]
        }
        if len(os.Args) > 2 {
                config.Password = os.Args[2]
        }
        if len(os.Args) > 3 {
                if interval, errAtoi := strconv.Atoi(os.Args[3]); errAtoi == nil && interval > 0 {
                        config.IntervalSeconds = interval
                }
        }

        // Create TUI and bot
        tui := NewTUI()
        botInstance := NewChatBot(config, tui)

        // Setup signal handling
        sigChan := make(chan os.Signal, 1)
        signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
        go func() {
                sig := <-sigChan
                botInstance.log("🛑 Received signal: %v. Initiating graceful shutdown...", sig)
                botInstance.Stop()
                tui.Stop() // This will close log files
                fmt.Println("\n👋 Goodbye! Logs saved to logs/ directory.")
                os.Exit(0)
        }()

        // Start the bot in a goroutine
        go func() {
                // Add a small delay to ensure TUI is ready
                time.Sleep(500 * time.Millisecond)
                if errStart := botInstance.Start(); errStart != nil {
                        tui.AddLogMessage(fmt.Sprintf("❌ CRITICAL: Failed to start bot: %v", errStart))
                }
        }()

        // Set focus to input field and ensure it's active
        tui.app.SetFocus(tui.inputField)

        // Run the TUI (this blocks until the application exits)
        if err := tui.Run(); err != nil {
                fmt.Printf("❌ TUI Error: %v\n", err)
                os.Exit(1)
        }
}
