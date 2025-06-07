package main

import (
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rivo/tview"
)

// IntervalConfig holds the random interval configuration
type IntervalConfig struct {
	From int `json:"from"`
	To   int `json:"to"`
}

// LoginAction holds the login action configuration
type LoginAction struct {
	LoginActionEnabled   bool     `json:"loginActionEnabled"`
	WaitSeconds         int      `json:"waitSeconds"`
	TimeBetweenCommands int      `json:"timeBetweenCommands"`
	Commands            []string `json:"commands"`
}

// Config holds the bot's configuration
type Config struct {
	Username        string           `json:"username"`
	Password        string           `json:"password"`
	IntervalSeconds *IntervalConfig  `json:"intervalSeconds"`
	LoginAction     *LoginAction     `json:"loginAction,omitempty"`
	Messages        []string         `json:"messages"`
	BaseURL         string           `json:"baseUrl,omitempty"`
	WebSocketURL    string           `json:"webSocketUrl,omitempty"`
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
	intervalConfig  *IntervalConfig
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
	// Login action functionality
	loginAction     *LoginAction
	loginActionDone bool
	loginActionMutex sync.Mutex
}
