package main

import (
	"encoding/binary"
	"testing"
)

func create16ByteEvent(evType, evCode uint16, evValue int32) []byte {
	buf := make([]byte, 16)
	binary.LittleEndian.PutUint32(buf[0:4], 1700000000) // sec
	binary.LittleEndian.PutUint32(buf[4:8], 500000)     // usec
	binary.LittleEndian.PutUint16(buf[8:10], evType)
	binary.LittleEndian.PutUint16(buf[10:12], evCode)
	binary.LittleEndian.PutUint32(buf[12:16], uint32(evValue))
	return buf
}

func create24ByteEvent(evType, evCode uint16, evValue int32) []byte {
	buf := make([]byte, 24)
	binary.LittleEndian.PutUint64(buf[0:8], 1700000000) // sec
	binary.LittleEndian.PutUint64(buf[8:16], 500000)    // usec
	binary.LittleEndian.PutUint16(buf[16:18], evType)
	binary.LittleEndian.PutUint16(buf[18:20], evCode)
	binary.LittleEndian.PutUint32(buf[20:24], uint32(evValue))
	return buf
}

func TestDetectEventStep(t *testing.T) {
	ev16 := create16ByteEvent(EV_ABS, ABS_X, 500)
	if step := DetectEventStep(ev16, len(ev16)); step != 16 {
		t.Errorf("Expected step 16, got %d", step)
	}

	ev24 := create24ByteEvent(EV_ABS, ABS_X, 500)
	if step := DetectEventStep(ev24, len(ev24)); step != 24 {
		t.Errorf("Expected step 24, got %d", step)
	}

	// Default fallback
	if step := DetectEventStep([]byte{1, 2, 3}, 3); step != 16 {
		t.Errorf("Expected fallback 16, got %d", step)
	}
}

func TestParseInputEvents16Byte(t *testing.T) {
	var stream []byte
	stream = append(stream, create16ByteEvent(EV_ABS, ABS_MT_POSITION_X, 600)...)
	stream = append(stream, create16ByteEvent(EV_ABS, ABS_MT_POSITION_Y, 800)...)
	stream = append(stream, create16ByteEvent(EV_SYN, 0, 0)...)

	events := ParseInputEvents(stream, len(stream), "/dev/input/event1")
	if len(events) != 3 {
		t.Fatalf("Expected 3 events, got %d", len(events))
	}

	if events[0].EvType != EV_ABS || events[0].EvCode != ABS_MT_POSITION_X || events[0].EvValue != 600 {
		t.Errorf("Event 0 unexpected: %+v", events[0])
	}
	if events[1].EvType != EV_ABS || events[1].EvCode != ABS_MT_POSITION_Y || events[1].EvValue != 800 {
		t.Errorf("Event 1 unexpected: %+v", events[1])
	}
	if events[2].EvType != EV_SYN || events[2].EvCode != 0 {
		t.Errorf("Event 2 unexpected: %+v", events[2])
	}
}

func TestParseInputEvents24Byte(t *testing.T) {
	var stream []byte
	stream = append(stream, create24ByteEvent(EV_KEY, KEY_POWER, 1)...)
	stream = append(stream, create24ByteEvent(EV_SYN, 0, 0)...)

	events := ParseInputEvents(stream, len(stream), "/dev/input/event0")
	if len(events) != 2 {
		t.Fatalf("Expected 2 events, got %d", len(events))
	}

	if events[0].EvType != EV_KEY || events[0].EvCode != KEY_POWER || events[0].EvValue != 1 {
		t.Errorf("Event 0 unexpected: %+v", events[0])
	}
}

func TestIsPowerKeyEvent(t *testing.T) {
	tests := []struct {
		name     string
		ev       RawEventMsg
		expected bool
	}{
		{"Power down", RawEventMsg{EvType: EV_KEY, EvCode: KEY_POWER, EvValue: 1}, true},
		{"Power up", RawEventMsg{EvType: EV_KEY, EvCode: KEY_POWER, EvValue: 0}, false},
		{"Sleep down", RawEventMsg{EvType: EV_KEY, EvCode: KEY_SLEEP, EvValue: 1}, true},
		{"Wakeup down", RawEventMsg{EvType: EV_KEY, EvCode: KEY_WAKEUP, EvValue: 1}, true},
		{"Touch down", RawEventMsg{EvType: EV_KEY, EvCode: BTN_TOUCH, EvValue: 1}, false},
		{"Abs event", RawEventMsg{EvType: EV_ABS, EvCode: KEY_POWER, EvValue: 1}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsPowerKeyEvent(tt.ev); got != tt.expected {
				t.Errorf("IsPowerKeyEvent() = %v, expected %v", got, tt.expected)
			}
		})
	}
}

func TestIsTouchEvent(t *testing.T) {
	if !IsTouchEvent(RawEventMsg{EvType: EV_ABS, EvCode: ABS_X}) {
		t.Error("EV_ABS should be identified as touch event")
	}
	if !IsTouchEvent(RawEventMsg{EvType: EV_KEY, EvCode: BTN_TOUCH}) {
		t.Error("BTN_TOUCH should be identified as touch event")
	}
	if !IsTouchEvent(RawEventMsg{EvType: EV_KEY, EvCode: BTN_LEFT}) {
		t.Error("BTN_LEFT should be identified as touch event")
	}
	if IsTouchEvent(RawEventMsg{EvType: EV_KEY, EvCode: KEY_POWER}) {
		t.Error("KEY_POWER should not be identified as touch event")
	}
}

func TestIsExplicitTouchRelease(t *testing.T) {
	tests := []struct {
		name     string
		ev       RawEventMsg
		expected bool
	}{
		{"BTN_TOUCH release", RawEventMsg{EvType: EV_KEY, EvCode: BTN_TOUCH, EvValue: 0}, true},
		{"BTN_TOUCH press", RawEventMsg{EvType: EV_KEY, EvCode: BTN_TOUCH, EvValue: 1}, false},
		{"ABS_MT_TRACKING_ID release", RawEventMsg{EvType: EV_ABS, EvCode: ABS_MT_TRACKING_ID, EvValue: -1}, true},
		{"ABS_MT_TRACKING_ID active", RawEventMsg{EvType: EV_ABS, EvCode: ABS_MT_TRACKING_ID, EvValue: 12}, false},
		{"ABS_PRESSURE zero", RawEventMsg{EvType: EV_ABS, EvCode: ABS_PRESSURE, EvValue: 0}, true},
		{"ABS_MT_TOUCH_MAJOR zero", RawEventMsg{EvType: EV_ABS, EvCode: ABS_MT_TOUCH_MAJOR, EvValue: 0}, true},
		{"Normal coordinate", RawEventMsg{EvType: EV_ABS, EvCode: ABS_MT_POSITION_X, EvValue: 400}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsExplicitTouchRelease(tt.ev); got != tt.expected {
				t.Errorf("IsExplicitTouchRelease() = %v, expected %v", got, tt.expected)
			}
		})
	}
}

func TestExtractCoordinates(t *testing.T) {
	curX, curY := int32(100), int32(200)

	// ABS_MT_POSITION_X
	newX, newY, ok := ExtractCoordinates(RawEventMsg{EvType: EV_ABS, EvCode: ABS_MT_POSITION_X, EvValue: 550}, curX, curY)
	if !ok || newX != 550 || newY != 200 {
		t.Errorf("ABS_MT_POSITION_X failed: got (%d, %d, %v)", newX, newY, ok)
	}

	// ABS_MT_POSITION_Y
	newX, newY, ok = ExtractCoordinates(RawEventMsg{EvType: EV_ABS, EvCode: ABS_MT_POSITION_Y, EvValue: 750}, 550, curY)
	if !ok || newX != 550 || newY != 750 {
		t.Errorf("ABS_MT_POSITION_Y failed: got (%d, %d, %v)", newX, newY, ok)
	}

	// Non-coordinate event
	newX, newY, ok = ExtractCoordinates(RawEventMsg{EvType: EV_SYN, EvCode: 0, EvValue: 0}, 550, 750)
	if ok || newX != 550 || newY != 750 {
		t.Errorf("Non-coordinate event should return ok=false: got (%d, %d, %v)", newX, newY, ok)
	}
}
