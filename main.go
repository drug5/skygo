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
        "sync/atomic"
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
        onlineHours  int32  // Use atomic
        onlineMinutes int32 // Use atomic
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
        // Performance optimizations
        logChan         chan string      // Async logging
        chatChan        chan ChatMessage // Async chat messages
        updateChan      chan func()      // Async UI updates
}

// ChatMessage represents a chat message for async processing
type ChatMessage struct {
        Timestamp string
        Username  string
        Message   string
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
        wsMutex         sync.RWMutex // Use RWMutex for better read performance
        currentRoom     string
        currentRoomName string
        messageTimer    *time.Timer
        timerMutex      sync.Mutex
        tui             *TUI
        isConnected     int32 // Use atomic for connection status
        connMutex       sync.RWMutex
        reconnectCount  int
        lastReconnect   time.Time
        // Autopick functionality
        autopickEnabled int32 // Use atomic
        userPositions   sync.Map // Use sync.Map for better concurrent access
        droppedItems    sync.Map // Use sync.Map for better concurrent access
        ownPosition     Position
        positionMutex   sync.RWMutex
        // Room fields - optimized
        roomFields      []RoomField
        availableCache  []Position // Cache available positions
        fieldsMutex     sync.RWMutex
        cacheValid      int32 // Atomic flag for cache validity
        // Performance monitoring
        moveQueue       chan MoveCommand     // Buffered channel for move commands
        actionQueue     chan ActionCommand   // Buffered channel for actions
        fastMode        int32               // Atomic flag for fast mode
}

// MoveCommand represents a move operation
type MoveCommand struct {
        X, Y int
        Callback func(error)
}

// ActionCommand represents an action operation
type ActionCommand struct {
        Type string
        Data interface{}
        Callback func(error)
}

// Position represents x,y coordinates
type Position struct {
        X int `json:"x"`
        Y int `json:"y"`
}

// DroppedItem represents an item that was dropped and needs to be picked up
type DroppedItem struct {
        ID       int      `json:"id"`
        Name     string   `json:"name"`
        Position Position `json:"position"`
        DroppedBy string  `json:"droppedBy"`
        DroppedAt time.Time `json:"droppedAt"`
}

// RoomField represents a field in the room that can potentially be moved to
type RoomField struct {
        X          int    `json:"x"`
        Y          int    `json:"y"`
        State      string `json:"state"`
        IsWalkable bool   `json:"isWalkable"`
}

// NewTUI creates a new terminal user interface with performance optimizations
func NewTUI() *TUI {
        tui := &TUI{
                app:   tview.NewApplication(),
                users: make(map[string]bool),
                // Initialize input history
                inputHistory: make([]string, 0),
                historyIndex: 0,
                currentInput: "",
                // Performance channels
                logChan:    make(chan string, 1000),      // Large buffer for async logging
                chatChan:   make(chan ChatMessage, 500),  // Buffer for chat messages
                updateChan: make(chan func(), 100),       // Buffer for UI updates
        }

        // Setup logging first
        if err := tui.setupLogging(); err != nil {
                fmt.Printf("❌ Failed to setup logging: %v\n", err)
                // Continue anyway, just without file logging
        } else {
                fmt.Println("📁 Logging initialized - saving to logs/ directory")
        }

        // Start async processors
        go tui.processLogs()
        go tui.processChatMessages()
        go tui.processUIUpdates()

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

// processLogs handles async log processing
func (tui *TUI) processLogs() {
        for logMsg := range tui.logChan {
                tui.writeToRawLog(logMsg)
                
                tui.app.QueueUpdateDraw(func() {
                        tui.logMutex.Lock()
                        timeStr := time.Now().Format("15:04:05.000")
                        formattedMsg := fmt.Sprintf("[gray]%s[white] %s\n", timeStr, logMsg)
                        fmt.Fprint(tui.logView, formattedMsg)
                        tui.logView.ScrollToEnd()
                        tui.logMutex.Unlock()
                })
        }
}

// processChatMessages handles async chat message processing
func (tui *TUI) processChatMessages() {
        for msg := range tui.chatChan {
                tui.writeToChatLog(msg.Timestamp, msg.Username, msg.Message)
                
                tui.app.QueueUpdateDraw(func() {
                        tui.chatMutex.Lock()
                        timeStr := msg.Timestamp
                        if timeStr == "" {
                                timeStr = time.Now().Format("15:04:05")
                        }

                        var formattedMsg string
                        if msg.Username != "" {
                                formattedMsg = fmt.Sprintf("[gray]%s[white] [yellow]<%s>[white] %s\n", timeStr, msg.Username, msg.Message)
                        } else {
                                formattedMsg = fmt.Sprintf("[gray]%s[white] [blue]***[white] %s\n", timeStr, msg.Message)
                        }

                        fmt.Fprint(tui.chatView, formattedMsg)
                        tui.chatView.ScrollToEnd()
                        tui.chatMutex.Unlock()
                })
        }
}

// processUIUpdates handles async UI updates
func (tui *TUI) processUIUpdates() {
        for updateFunc := range tui.updateChan {
                tui.app.QueueUpdateDraw(updateFunc)
        }
}

// AddChatMessage adds a formatted chat message to the chat pane (async)
func (tui *TUI) AddChatMessage(timestamp, username, message string) {
        select {
        case tui.chatChan <- ChatMessage{Timestamp: timestamp, Username: username, Message: message}:
        default:
                // Channel full, skip to avoid blocking
        }
}

// AddLogMessage adds a log message to the log pane (async)
func (tui *TUI) AddLogMessage(message string) {
        select {
        case tui.logChan <- message:
        default:
                // Channel full, skip to avoid blocking
        }
}

// UpdateUsers updates the user list in the user pane
func (tui *TUI) UpdateUsers(users []string) {
        updateFunc := func() {
                tui.userMutex.Lock()
                defer tui.userMutex.Unlock()

                tui.userView.Clear()
                fmt.Fprintf(tui.userView, "[yellow]Room: %s[white]\n\n", tui.currentRoom)

                for _, user := range users {
                        fmt.Fprintf(tui.userView, "[green]● [white]%s\n", user)
                }
        }
        
        select {
        case tui.updateChan <- updateFunc:
        default:
                // Channel full, execute directly
                tui.app.QueueUpdateDraw(updateFunc)
        }
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

        // Close async channels
        close(tui.logChan)
        close(tui.chatChan)
        close(tui.updateChan)
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

// UpdateOnlineTime updates the tracked online time from server data (atomic)
func (tui *TUI) UpdateOnlineTime(hours, minutes int) {
        atomic.StoreInt32(&tui.onlineHours, int32(hours))
        atomic.StoreInt32(&tui.onlineMinutes, int32(minutes))
}

// GetOnlineTime returns the current online time (atomic)
func (tui *TUI) GetOnlineTime() (int, int) {
        return int(atomic.LoadInt32(&tui.onlineHours)), int(atomic.LoadInt32(&tui.onlineMinutes))
}

// UpdateStatusBar updates the comprehensive status bar with all information
func (tui *TUI) UpdateStatusBar(username string, connected bool, roomName, roomID, autopickStatus string) {
        updateFunc := func() {
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
        }
        
        select {
        case tui.updateChan <- updateFunc:
        default:
                // Channel full, execute directly
                tui.app.QueueUpdateDraw(updateFunc)
        }
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

// NewChatBot creates a new ChatBot instance with performance optimizations
func NewChatBot(config *Config, tui *TUI) *ChatBot {
        bot := &ChatBot{
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
                reconnectCount:  0,
                ownPosition:     Position{X: 0, Y: 0},
                roomFields:      make([]RoomField, 0),
                availableCache:  make([]Position, 0),
                // Performance queues
                moveQueue:   make(chan MoveCommand, 100),   // Buffered move queue
                actionQueue: make(chan ActionCommand, 100), // Buffered action queue
        }

        // Start async processors
        go bot.processMoveQueue()
        go bot.processActionQueue()

        return bot
}

// processMoveQueue processes move commands asynchronously
func (bot *ChatBot) processMoveQueue() {
        for moveCmd := range bot.moveQueue {
                err := bot.moveToPositionDirect(moveCmd.X, moveCmd.Y)
                if moveCmd.Callback != nil {
                        moveCmd.Callback(err)
                }
                
                // Fast mode: minimal delay between moves
                if atomic.LoadInt32(&bot.fastMode) > 0 {
                        time.Sleep(25 * time.Millisecond) // Ultra-fast mode
                } else {
                        time.Sleep(50 * time.Millisecond) // Normal fast mode
                }
        }
}

// processActionQueue processes action commands asynchronously  
func (bot *ChatBot) processActionQueue() {
        for actionCmd := range bot.actionQueue {
                var err error
                switch actionCmd.Type {
                case "pickup":
                        if itemID, ok := actionCmd.Data.(int); ok {
                                err = bot.pickupItemDirect(itemID)
                        }
                case "chat":
                        if message, ok := actionCmd.Data.(string); ok {
                                err = bot.sendMessageDirect(message)
                        }
                }
                
                if actionCmd.Callback != nil {
                        actionCmd.Callback(err)
                }
        }
}

// log is a helper function to log to the TUI instead of stdout (async)
func (bot *ChatBot) log(format string, args ...interface{}) {
        message := fmt.Sprintf(format, args...)
        if bot.tui != nil {
                bot.tui.AddLogMessage(message)
        }
}

// setConnectionStatus safely updates connection status (atomic)
func (bot *ChatBot) setConnectionStatus(connected bool) {
        var connValue int32
        if connected {
                connValue = 1
        }
        
        wasConnected := atomic.SwapInt32(&bot.isConnected, connValue) == 1

        if connected && !wasConnected {
                bot.reconnectCount = 0
                bot.updateFullStatus()
                bot.log("🟢 Connection status: CONNECTED")
        } else if !connected && wasConnected {
                bot.updateFullStatus()
                bot.log("🔴 Connection status: DISCONNECTED")
        }
}

// isConnectionHealthy checks if connection is healthy (atomic)
func (bot *ChatBot) isConnectionHealthy() bool {
        return atomic.LoadInt32(&bot.isConnected) == 1 && bot.wsConn != nil
}

// isAutopickEnabled safely checks if autopick is enabled (atomic)
func (bot *ChatBot) isAutopickEnabled() bool {
        return atomic.LoadInt32(&bot.autopickEnabled) == 1
}

// setAutopickEnabled safely sets autopick status (atomic)
func (bot *ChatBot) setAutopickEnabled(enabled bool) {
        var value int32
        if enabled {
                value = 1
        }
        atomic.StoreInt32(&bot.autopickEnabled, value)

        status := "DISABLED"
        if enabled {
                status = "ENABLED"
        }
        bot.log("🤖 Autopick %s", status)
        bot.updateFullStatus()
}

// enableFastMode enables ultra-fast operation mode
func (bot *ChatBot) enableFastMode(enabled bool) {
        var value int32
        if enabled {
                value = 1
        }
        atomic.StoreInt32(&bot.fastMode, value)
        
        mode := "NORMAL"
        if enabled {
                mode = "ULTRA-FAST"
        }
        bot.log("🚀 Performance mode: %s", mode)
}

// updateUserPosition updates a user's position (optimized with sync.Map)
func (bot *ChatBot) updateUserPosition(username string, x, y int) {
        pos := Position{X: x, Y: y}
        
        if username == bot.username {
                bot.positionMutex.Lock()
                bot.ownPosition = pos
                bot.positionMutex.Unlock()
                bot.log("📍 Own position updated: (%d, %d)", x, y)
                
                // Invalidate position cache
                atomic.StoreInt32(&bot.cacheValid, 0)
        } else {
                if bot.isAutopickEnabled() {
                        // Check if user moved and handle item opportunities
                        if oldPosVal, exists := bot.userPositions.LoadAndDelete(username); exists {
                                if oldPos, ok := oldPosVal.(Position); ok && (oldPos.X != x || oldPos.Y != y) {
                                        go bot.checkItemOpportunitiesFast(username, oldPos)
                                }
                        }
                }
                bot.userPositions.Store(username, pos)
                
                // Invalidate position cache
                atomic.StoreInt32(&bot.cacheValid, 0)
        }
}

// checkItemOpportunitiesFast optimized version for faster item pickup
func (bot *ChatBot) checkItemOpportunitiesFast(username string, oldPos Position) {
        var itemsToCheck []DroppedItem
        
        // Collect items to check
        bot.droppedItems.Range(func(key, value interface{}) bool {
                if item, ok := value.(DroppedItem); ok {
                        if item.DroppedBy == username && item.Position.X == oldPos.X && item.Position.Y == oldPos.Y {
                                itemsToCheck = append(itemsToCheck, item)
                        }
                }
                return true
        })

        // Process items with minimal delay
        for _, item := range itemsToCheck {
                bot.log("⚡ User %s moved away from %s, immediate pickup attempt", username, item.Name)

                // Queue move and pickup as fast as possible
                moveDone := make(chan error, 1)
                bot.queueMove(item.Position.X, item.Position.Y, func(err error) {
                        moveDone <- err
                })

                // Wait for move with timeout
                select {
                case err := <-moveDone:
                        if err != nil {
                                bot.log("❌ Failed to move to item position: %v", err)
                                continue
                        }
                case <-time.After(200 * time.Millisecond):
                        bot.log("⏱️ Move timeout for item %s", item.Name)
                        continue
                }

                // Minimal wait then pickup
                time.Sleep(25 * time.Millisecond)

                pickupDone := make(chan error, 1)
                bot.queueAction("pickup", item.ID, func(err error) {
                        pickupDone <- err
                })

                // Wait for pickup
                select {
                case err := <-pickupDone:
                        if err != nil {
                                bot.log("❌ Failed to pick up item: %v", err)
                                continue
                        }
                case <-time.After(200 * time.Millisecond):
                        bot.log("⏱️ Pickup timeout for item %s", item.Name)
                        continue
                }

                // Remove from tracking
                bot.droppedItems.Delete(item.ID)
                bot.log("✅ Successfully picked up %s (ID:%d)", item.Name, item.ID)
                bot.tui.AddChatMessage("", "", fmt.Sprintf("🤖 Autopicked: %s", item.Name))
                break // Only pick up one item at a time
        }
}

// queueMove queues a move command for async processing
func (bot *ChatBot) queueMove(x, y int, callback func(error)) {
        select {
        case bot.moveQueue <- MoveCommand{X: x, Y: y, Callback: callback}:
        default:
                // Queue full, execute directly
                go func() {
                        err := bot.moveToPositionDirect(x, y)
                        if callback != nil {
                                callback(err)
                        }
                }()
        }
}

// queueAction queues an action command for async processing
func (bot *ChatBot) queueAction(actionType string, data interface{}, callback func(error)) {
        select {
        case bot.actionQueue <- ActionCommand{Type: actionType, Data: data, Callback: callback}:
        default:
                // Queue full, execute directly based on type
                go func() {
                        var err error
                        switch actionType {
                        case "pickup":
                                if itemID, ok := data.(int); ok {
                                        err = bot.pickupItemDirect(itemID)
                                }
                        case "chat":
                                if message, ok := data.(string); ok {
                                        err = bot.sendMessageDirect(message)
                                }
                        }
                        if callback != nil {
                                callback(err)
                        }
                }()
        }
}

// handleItemDropped processes when an item is dropped by a user (optimized)
func (bot *ChatBot) handleItemDropped(itemID int, itemName string, x, y int, droppedByUsername string) {
        if !bot.isAutopickEnabled() {
                return
        }

        droppedItem := DroppedItem{
                ID:       itemID,
                Name:     itemName,
                Position: Position{X: x, Y: y},
                DroppedBy: droppedByUsername,
                DroppedAt: time.Now(),
        }

        bot.droppedItems.Store(itemID, droppedItem)
        bot.log("📦 Item dropped: %s (ID:%d) at (%d,%d) by %s", itemName, itemID, x, y, droppedByUsername)

        // Start ultra-fast monitoring
        go bot.monitorDroppedItemUltraFast(droppedItem)
}

// monitorDroppedItemUltraFast watches a dropped item with ultra-minimal delays
func (bot *ChatBot) monitorDroppedItemUltraFast(item DroppedItem) {
        // Ultra-short initial wait
        time.Sleep(25 * time.Millisecond)

        maxWaitTime := 3 * time.Second // Reduced from 5 seconds
        startTime := time.Now()
        checkInterval := 25 * time.Millisecond // Ultra-fast checking

        for time.Since(startTime) < maxWaitTime {
                // Check if item still exists
                if _, exists := bot.droppedItems.Load(item.ID); !exists {
                        return
                }

                // Check if dropper moved away
                dropperPosVal, dropperExists := bot.userPositions.Load(item.DroppedBy)
                
                if !dropperExists {
                        // Dropper disappeared, immediate pickup
                        bot.executeImmediatePickup(item)
                        return
                }

                if dropperPos, ok := dropperPosVal.(Position); ok {
                        if dropperPos.X != item.Position.X || dropperPos.Y != item.Position.Y {
                                // Dropper moved, immediate pickup
                                bot.executeImmediatePickup(item)
                                return
                        }
                }

                time.Sleep(checkInterval)
        }

        bot.log("⏱️ Ultra-fast timeout for %s", item.Name)
}

// executeImmediatePickup performs immediate pickup with minimal delays
func (bot *ChatBot) executeImmediatePickup(item DroppedItem) {
        bot.log("🏃 User %s moved away from %s, executing immediate pickup", item.DroppedBy, item.Name)

        // Ultra-fast move
        if err := bot.moveToPositionDirect(item.Position.X, item.Position.Y); err != nil {
                bot.log("❌ Failed to move to item position: %v", err)
                return
        }

        // Minimal wait
        time.Sleep(50 * time.Millisecond)

        // Ultra-fast pickup
        if err := bot.pickupItemDirect(item.ID); err != nil {
                bot.log("❌ Failed to pick up item: %v", err)
                return
        }

        bot.droppedItems.Delete(item.ID)
        bot.log("✅ Successfully picked up %s (ID:%d)", item.Name, item.ID)
        bot.tui.AddChatMessage("", "", fmt.Sprintf("🤖 Autopicked: %s", item.Name))
}

// handleItemPickedUp processes when an item is picked up (optimized)
func (bot *ChatBot) handleItemPickedUp(itemID int) {
        if itemVal, exists := bot.droppedItems.LoadAndDelete(itemID); exists {
                if item, ok := itemVal.(DroppedItem); ok {
                        bot.log("📦 Item %s (ID:%d) was picked up by someone", item.Name, itemID)
                }
        }
}

// moveToPositionDirect sends a move command directly (optimized)
func (bot *ChatBot) moveToPositionDirect(x, y int) error {
        if !bot.isConnectionHealthy() {
                return fmt.Errorf("WebSocket connection not healthy")
        }

        // Pre-marshal the move payload for better performance
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

        bot.wsMutex.Lock()
        if bot.wsConn == nil {
                bot.wsMutex.Unlock()
                return fmt.Errorf("WebSocket connection not established")
        }

        // Reduced write deadline for faster operations
        bot.wsConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
        err = bot.wsConn.WriteMessage(websocket.TextMessage, msgBytes)
        bot.wsMutex.Unlock()

        if err != nil {
                bot.setConnectionStatus(false)
                return fmt.Errorf("failed to send move command: %v", err)
        }

        // Update position
        bot.updateUserPosition(bot.username, x, y)
        return nil
}

// moveToPosition queued version for normal use
func (bot *ChatBot) moveToPosition(x, y int) error {
        done := make(chan error, 1)
        bot.queueMove(x, y, func(err error) {
                done <- err
        })
        
        select {
        case err := <-done:
                return err
        case <-time.After(1 * time.Second):
                return fmt.Errorf("move operation timeout")
        }
}

// parseRoomFields parses and stores the room field data (optimized)
func (bot *ChatBot) parseRoomFields(fieldsData []interface{}) {
        bot.fieldsMutex.Lock()
        defer bot.fieldsMutex.Unlock()

        bot.roomFields = make([]RoomField, 0, len(fieldsData)) // Pre-allocate capacity

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

        // Invalidate cache
        atomic.StoreInt32(&bot.cacheValid, 0)
        bot.log("🗺️ Parsed %d room fields", len(bot.roomFields))
}

// getAvailablePositions returns cached or computed available positions
func (bot *ChatBot) getAvailablePositions() []Position {
        // Check if cache is valid
        if atomic.LoadInt32(&bot.cacheValid) == 1 {
                bot.fieldsMutex.RLock()
                result := make([]Position, len(bot.availableCache))
                copy(result, bot.availableCache)
                bot.fieldsMutex.RUnlock()
                return result
        }

        // Rebuild cache
        bot.fieldsMutex.Lock()
        defer bot.fieldsMutex.Unlock()

        var available []Position
        occupiedPositions := make(map[Position]bool)

        // Collect occupied positions
        bot.userPositions.Range(func(key, value interface{}) bool {
                if pos, ok := value.(Position); ok {
                        occupiedPositions[pos] = true
                }
                return true
        })

        // Add own position
        bot.positionMutex.RLock()
        occupiedPositions[bot.ownPosition] = true
        bot.positionMutex.RUnlock()

        // Find available fields
        for _, field := range bot.roomFields {
                if field.State == "open" && field.IsWalkable {
                        pos := Position{X: field.X, Y: field.Y}
                        if !occupiedPositions[pos] {
                                available = append(available, pos)
                        }
                }
        }

        // Update cache
        bot.availableCache = make([]Position, len(available))
        copy(bot.availableCache, available)
        atomic.StoreInt32(&bot.cacheValid, 1)

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

// moveMultipleTimes moves the bot to random positions multiple times with ultra-fast mode
func (bot *ChatBot) moveMultipleTimes(count int) error {
        if count <= 0 {
                return fmt.Errorf("move count must be positive")
        }

        bot.log("🎲 Starting ultra-fast multiple moves: %d times", count)
        bot.tui.AddChatMessage("", "", fmt.Sprintf("🚀 Ultra-fast moving %d times...", count))

        // Enable ultra-fast mode temporarily
        bot.enableFastMode(true)
        defer bot.enableFastMode(false)

        successfulMoves := 0
        var wg sync.WaitGroup
        moveChan := make(chan Position, count)
        errorChan := make(chan error, count)

        // Pre-calculate all positions
        go func() {
                defer close(moveChan)
                for i := 0; i < count; i++ {
                        availablePositions := bot.getAvailablePositions()
                        if len(availablePositions) == 0 {
                                errorChan <- fmt.Errorf("no available positions for move %d", i+1)
                                break
                        }
                        randomIndex := time.Now().UnixNano() % int64(len(availablePositions))
                        moveChan <- availablePositions[randomIndex]
                }
        }()

        // Process moves in parallel batches
        const batchSize = 5
        for i := 0; i < count; i += batchSize {
                batchEnd := i + batchSize
                if batchEnd > count {
                        batchEnd = count
                }

                for j := i; j < batchEnd; j++ {
                        wg.Add(1)
                        go func(moveNum int) {
                                defer wg.Done()
                                
                                select {
                                case pos := <-moveChan:
                                        if err := bot.moveToPositionDirect(pos.X, pos.Y); err != nil {
                                                errorChan <- fmt.Errorf("move %d failed: %v", moveNum+1, err)
                                                return
                                        }
                                        successfulMoves++
                                        bot.log("🎲 Ultra-fast move %d/%d to (%d, %d)", moveNum+1, count, pos.X, pos.Y)
                                case <-time.After(500 * time.Millisecond):
                                        errorChan <- fmt.Errorf("move %d timeout", moveNum+1)
                                        return
                                }
                        }(j)
                }

                wg.Wait()
                
                // Ultra-minimal delay between batches
                if i+batchSize < count {
                        time.Sleep(10 * time.Millisecond)
                }
        }

        // Check for any errors
        select {
        case err := <-errorChan:
                bot.log("⚠️ %v", err)
        default:
        }

        bot.log("✅ Completed %d/%d ultra-fast moves", successfulMoves, count)
        bot.tui.AddChatMessage("", "", fmt.Sprintf("🚀 Completed %d/%d ultra-fast moves", successfulMoves, count))
        return nil
}

// pickupItemDirect sends a pickup command directly (optimized)
func (bot *ChatBot) pickupItemDirect(itemID int) error {
        if !bot.isConnectionHealthy() {
                return fmt.Errorf("WebSocket connection not healthy")
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

        bot.wsMutex.Lock()
        if bot.wsConn == nil {
                bot.wsMutex.Unlock()
                return fmt.Errorf("WebSocket connection not established")
        }

        bot.wsConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
        err = bot.wsConn.WriteMessage(websocket.TextMessage, msgBytes)
        bot.wsMutex.Unlock()

        if err != nil {
                bot.setConnectionStatus(false)
                return fmt.Errorf("failed to send pickup command: %v", err)
        }

        return nil
}

// pickupItem queued version for normal use
func (bot *ChatBot) pickupItem(itemID int) error {
        done := make(chan error, 1)
        bot.queueAction("pickup", itemID, func(err error) {
                done <- err
        })
        
        select {
        case err := <-done:
                return err
        case <-time.After(1 * time.Second):
                return fmt.Errorf("pickup operation timeout")
        }
}

// updateFullStatus updates the complete status bar
func (bot *ChatBot) updateFullStatus() {
        autopickStatus := "OFF"
        if bot.isAutopickEnabled() {
                autopickStatus = "ON"
        }
        bot.tui.UpdateStatusBar(bot.username, bot.isConnectionHealthy(), bot.currentRoomName, bot.currentRoom, autopickStatus)
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

// sendMessageDirect sends a message directly (optimized)
func (bot *ChatBot) sendMessageDirect(message string) error {
        if !bot.isConnectionHealthy() {
                return fmt.Errorf("WebSocket connection not healthy")
        }

        msgPayload := map[string]interface{}{
                "type": "chat",
                "data": map[string]interface{}{
                        "message": message,
                        "room":    bot.currentRoom,
                },
        }

        msgBytes, err := json.Marshal(msgPayload)
        if err != nil {
                return fmt.Errorf("failed to marshal message: %v", err)
        }

        bot.wsMutex.Lock()
        if bot.wsConn == nil {
                bot.wsMutex.Unlock()
                return fmt.Errorf("WebSocket connection not established")
        }

        bot.wsConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
        err = bot.wsConn.WriteMessage(websocket.TextMessage, msgBytes)
        bot.wsMutex.Unlock()

        if err != nil {
                bot.setConnectionStatus(false)
                return fmt.Errorf("failed to send message: %v", err)
        }

        return nil
}

// sendMessageInternal sends a message over WebSocket and optionally resets the periodic timer
func (bot *ChatBot) sendMessageInternal(message string, resetTimer bool) error {
        err := bot.sendMessageDirect(message)
        if err != nil {
                return err
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

// listenForMessages reads messages from WebSocket and processes them (optimized)
func (bot *ChatBot) listenForMessages() {
        bot.log("👂 Starting optimized message listener...")

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

                // Process message in goroutine for better performance
                go bot.processIncomingMessage(message)
        }
}

// processIncomingMessage processes incoming WebSocket messages asynchronously
func (bot *ChatBot) processIncomingMessage(message []byte) {
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
                                                        if username, usernameOk := client["username"].(string); usernameOk {
                                                                users = append(users, username)

                                                                // Track initial positions
                                                                if x, xOk := client["x"].(float64); xOk {
                                                                        if y, yOk := client["y"].(float64); yOk {
                                                                                bot.updateUserPosition(username, int(x), int(y))
                                                                        }
                                                                }

                                                                // Track our own position
                                                                if isPlayer, playerOk := client["isPlayer"].(bool); playerOk && isPlayer {
                                                                        if x, xOk := client["x"].(float64); xOk {
                                                                                if y, yOk := client["y"].(float64); yOk {
                                                                                        bot.updateUserPosition(bot.username, int(x), int(y))
                                                                                }
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
                        // Handle user joins (updates)
                        if updates, updatesOk := clientsData["updates"].([]interface{}); updatesOk {
                                for _, update := range updates {
                                        if client, clientOk := update.(map[string]interface{}); clientOk {
                                                // Extract user info
                                                var username string
                                                var userX, userY int

                                                if name, nameOk := client["username"].(string); nameOk {
                                                        username = name
                                                }
                                                if x, xOk := client["x"].(float64); xOk {
                                                        userX = int(x)
                                                }
                                                if y, yOk := client["y"].(float64); yOk {
                                                        userY = int(y)
                                                }

                                                // Update user position if we have coordinates
                                                if username != "" && (userX != 0 || userY != 0) {
                                                        bot.updateUserPosition(username, userX, userY)
                                                }

                                                // Check for new user join (not initial room data)
                                                if username != "" {
                                                        if _, hasRoom := messageData["room"]; !hasRoom {
                                                                bot.tui.AddUser(username)
                                                                bot.log("👤 User joined: %s", username)
                                                        }
                                                }

                                                // Handle chat messages from events within client updates
                                                if events, eventsOk := client["events"].([]interface{}); eventsOk {
                                                        for _, event := range events {
                                                                if eventData, eventOk := event.(map[string]interface{}); eventOk {
                                                                        if eventType, typeOk := eventData["type"].(string); typeOk && eventType == "chat" {
                                                                                if data, dataOk := eventData["data"].(map[string]interface{}); dataOk {
                                                                                        if username, usernameOk := data["username"].(string); usernameOk {
                                                                                                if message, messageOk := data["message"].(string); messageOk {
                                                                                                        bot.log("💬 Chat [%s]: %s", username, message)
                                                                                                        // Don't display your own messages again (they're already shown when sent)
                                                                                                        if username != bot.username {
                                                                                                                bot.tui.AddChatMessage("", username, message)
                                                                                                        }
                                                                                                }
                                                                                        }
                                                                                }
                                                                        }
                                                                }
                                                        }
                                                }

                                                // Handle item removal from user (item being dropped)
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

                                                                                                                        bot.handleItemDropped(int(itemID), itemName, itemX, itemY, username)
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
                        if _, removesOk := clientsData["removes"].([]interface{}); removesOk {
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

                                                                                                        // Remove from position tracking
                                                                                                        bot.userPositions.Delete(username)
                                                                                                        atomic.StoreInt32(&bot.cacheValid, 0) // Invalidate cache
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

// setupInputHandler configures the input field for commands and messages (optimized)
func (bot *ChatBot) setupInputHandler() {
        // Set up input capture for arrow keys (for command history)
        bot.tui.inputField.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
                switch event.Key() {
                case tcell.KeyUp:
                        // Get previous command from history
                        if historyItem := bot.tui.GetHistoryUp(); historyItem != "" {
                                bot.tui.inputField.SetText(historyItem)
                        }
                        return nil // Consume the event
                case tcell.KeyDown:
                        // Get next command from history
                        historyItem := bot.tui.GetHistoryDown()
                        bot.tui.inputField.SetText(historyItem)
                        return nil // Consume the event
                }
                return event // Pass through other events
        })

        bot.tui.inputField.SetDoneFunc(func(key tcell.Key) {
                if key == tcell.KeyEnter {
                        input := strings.TrimSpace(bot.tui.inputField.GetText())
                        if input == "" {
                                return
                        }

                        // Add to input history
                        bot.tui.AddToInputHistory(input)

                        // Clear input field immediately for better UX
                        bot.tui.inputField.SetText("")

                        // Handle commands and messages in goroutine to not block TUI
                        go func(message string) {
                                // Handle commands
                                if strings.HasPrefix(message, "/") {
                                        fields := strings.Fields(message)
                                        command := fields[0]

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
                                                bot.tui.AddChatMessage("", "", "/move [count] - Move to random field (optionally multiple times ultra-fast)")
                                                bot.tui.AddChatMessage("", "", "/fast - Toggle ultra-fast mode on/off")
                                        case "/status":
                                                wsStatus := "Disconnected"
                                                if bot.isConnectionHealthy() {
                                                        wsStatus = "Connected"
                                                }
                                                autopickStatus := "DISABLED"
                                                if bot.isAutopickEnabled() {
                                                        autopickStatus = "ENABLED"
                                                }
                                                fastModeStatus := "NORMAL"
                                                if atomic.LoadInt32(&bot.fastMode) == 1 {
                                                        fastModeStatus = "ULTRA-FAST"
                                                }
                                                hours, minutes := bot.tui.GetOnlineTime()
                                                bot.tui.AddChatMessage("", "", fmt.Sprintf("Username: %s", bot.username))
                                                bot.tui.AddChatMessage("", "", fmt.Sprintf("Current Room: %s (%s)", bot.currentRoomName, bot.currentRoom))
                                                bot.tui.AddChatMessage("", "", fmt.Sprintf("WebSocket: %s", wsStatus))
                                                bot.tui.AddChatMessage("", "", fmt.Sprintf("Online Time: %dh %dm", hours, minutes))
                                                bot.tui.AddChatMessage("", "", fmt.Sprintf("Message Interval: %v", bot.messageInterval))
                                                bot.tui.AddChatMessage("", "", fmt.Sprintf("Autopick: %s", autopickStatus))
                                                bot.tui.AddChatMessage("", "", fmt.Sprintf("Performance Mode: %s", fastModeStatus))
                                                bot.tui.AddChatMessage("", "", fmt.Sprintf("Reconnect Count: %d", bot.reconnectCount))
                                                if !bot.lastReconnect.IsZero() {
                                                        bot.tui.AddChatMessage("", "", fmt.Sprintf("Last Reconnect: %s", bot.lastReconnect.Format("15:04:05")))
                                                }

                                                // Show tracked items if autopick is enabled
                                                if bot.isAutopickEnabled() {
                                                        itemCount := 0
                                                        bot.droppedItems.Range(func(key, value interface{}) bool {
                                                                itemCount++
                                                                return true
                                                        })
                                                        bot.tui.AddChatMessage("", "", fmt.Sprintf("Tracked Items: %d", itemCount))
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
                                                // Check if a count was provided
                                                if len(fields) > 1 {
                                                        // Parse the count
                                                        if count, err := strconv.Atoi(fields[1]); err != nil {
                                                                bot.tui.AddChatMessage("", "", fmt.Sprintf("❌ Invalid move count: '%s'. Must be a positive number.", fields[1]))
                                                        } else if count <= 0 {
                                                                bot.tui.AddChatMessage("", "", "❌ Move count must be positive")
                                                        } else if count > 500 {
                                                                bot.tui.AddChatMessage("", "", "❌ Move count too high (max 500 for ultra-fast mode)")
                                                        } else {
                                                                // Perform multiple moves ultra-fast
                                                                go func() {
                                                                        if err := bot.moveMultipleTimes(count); err != nil {
                                                                                bot.tui.AddChatMessage("", "", fmt.Sprintf("❌ Multiple move failed: %v", err))
                                                                        }
                                                                }()
                                                        }
                                                } else {
                                                        // Single move (original behavior)
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
                                                }
                                        case "/autopick":
                                                currentStatus := bot.isAutopickEnabled()
                                                bot.setAutopickEnabled(!currentStatus)
                                                newStatus := "ENABLED"
                                                if !bot.isAutopickEnabled() {
                                                        newStatus = "DISABLED"
                                                }
                                                bot.tui.AddChatMessage("", "", fmt.Sprintf("🤖 Autopick %s", newStatus))
                                        case "/fast":
                                                currentMode := atomic.LoadInt32(&bot.fastMode) == 1
                                                bot.enableFastMode(!currentMode)
                                                newMode := "NORMAL"
                                                if atomic.LoadInt32(&bot.fastMode) == 1 {
                                                        newMode = "ULTRA-FAST"
                                                }
                                                bot.tui.AddChatMessage("", "", fmt.Sprintf("🚀 Performance mode: %s", newMode))
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

// sendKeepAlive sends WebSocket ping messages periodically (optimized)
func (bot *ChatBot) sendKeepAlive() {
        bot.log("💓 Starting optimized keep-alive sender (30 second interval)")
        ticker := time.NewTicker(30 * time.Second)
        defer ticker.Stop()

        for range ticker.C {
                if bot.isConnectionHealthy() {
                        bot.wsMutex.RLock()
                        if bot.wsConn != nil {
                                bot.wsConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
                                if err := bot.wsConn.WriteMessage(websocket.PingMessage, []byte("keepalive")); err != nil {
                                        bot.wsMutex.RUnlock()
                                        bot.log("❌ Failed to send ping: %v", err)
                                        bot.setConnectionStatus(false)
                                } else {
                                        bot.wsMutex.RUnlock()
                                        bot.log("💓 Keep-alive ping sent successfully")
                                }
                        } else {
                                bot.wsMutex.RUnlock()
                        }
                } else {
                        bot.log("⚠️ Skipping keep-alive ping - connection not healthy")
                }
        }
}

// startStatusUpdater keeps the status bar current time updated (optimized)
func (bot *ChatBot) startStatusUpdater() {
        bot.log("⏰ Starting optimized status updater (5 second interval)")
        ticker := time.NewTicker(5 * time.Second)
        defer ticker.Stop()

        for range ticker.C {
                // Update the status bar with current time (other info stays the same)
                bot.updateFullStatus()
        }
}

// Start initializes and runs the bot (optimized)
func (bot *ChatBot) Start() error {
        bot.log("🚀 Starting ultra-optimized Skyskraber chat bot...")
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

        bot.log("👂 Step 3: Starting optimized message listener...")
        go bot.listenForMessages()

        bot.log("🕐 Step 4: Starting periodic message sender...")
        go bot.sendPeriodicMessages()

        bot.log("💓 Step 5: Starting optimized keep-alive routine...")
        go bot.sendKeepAlive()

        bot.log("⏰ Step 6: Starting optimized status updater...")
        go bot.startStatusUpdater()

        bot.log("⌨️ Step 7: Setting up optimized input handler...")
        bot.setupInputHandler()

        bot.log("✅ Ultra-optimized bot started successfully! 🚀")
        bot.updateFullStatus()

        bot.tui.AddChatMessage("", "", "=== Ultra-Optimized Skyskraber Chat Bot Started ===")
        bot.tui.AddChatMessage("", "", "Type /help for available commands")
        bot.tui.AddChatMessage("", "", "Press Ctrl+C or type /quit to exit")
        bot.tui.AddChatMessage("", "", "📁 Logs are being saved to logs/ directory")
        bot.tui.AddChatMessage("", "", "🤖 Autopick is DISABLED by default - use /autopick to enable")
        bot.tui.AddChatMessage("", "", "🎲 Use /move to move to random open field")
        bot.tui.AddChatMessage("", "", "🚀 Use /move <number> to move multiple times ultra-fast (max 500)")
        bot.tui.AddChatMessage("", "", "🚀 Use /fast to toggle ultra-fast performance mode")
        bot.tui.AddChatMessage("", "", "⬆️⬇️ Use arrow keys to navigate input history")
        bot.tui.AddChatMessage("", "", "🚀 All operations are now ultra-optimized for maximum performance!")

        return nil
}

// Stop gracefully shuts down the bot (optimized)
func (bot *ChatBot) Stop() {
        bot.log("🛑 Stopping ultra-optimized bot...")

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

        // Close performance queues
        close(bot.moveQueue)
        close(bot.actionQueue)

        bot.log("📁 Closing log files...")
        bot.tui.closeLogFiles()
        bot.log("✅ Ultra-optimized bot stopped.")
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
