package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/gorilla/websocket"
)

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
