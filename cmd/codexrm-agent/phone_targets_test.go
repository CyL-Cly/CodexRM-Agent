package main

import (
	"slices"
	"testing"

	"github.com/CyL-Cly/CodexRM-Server/protocol"
)

func TestPairedPhoneTargets(t *testing.T) {
	devices := []protocol.ClientInfo{
		{ClientID: "phone-000001", ClientName: "Pixel", DeviceType: "android"},
		{ClientID: "browser", ClientName: "Browser", DeviceType: "mobile"},
		{ClientID: "phone-000002", ClientName: "Pixel", DeviceType: "android"},
		{ClientID: "phone-000003", ClientName: "\r\n", DeviceType: "android"},
		{ClientID: "phone-000004", ClientName: "nubia\nNX666J", DeviceType: "android"},
	}
	got := pairedPhoneTargets(devices, func(id string) bool {
		if id == "browser" {
			t.Fatal("queried P2P status for a non-Android device")
		}
		return id == "phone-000002" || id == "phone-000004"
	})
	want := []phoneTarget{
		{id: "phone-000002", name: "Pixel · 000002", direct: true},
		{id: "phone-000004", name: "nubia NX666J · 000004", direct: true},
		{id: "phone-000001", name: "Pixel · 000001"},
		{id: "phone-000003", name: "Android 手机 · 000003"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("targets = %+v, want %+v", got, want)
	}
}
