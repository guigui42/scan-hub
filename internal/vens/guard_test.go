package vens

import (
	"encoding/binary"
	"testing"
	"time"
)

func controlPacket(cmd uint32) []byte {
	p := make([]byte, 32)
	binary.BigEndian.PutUint32(p[0:], 32)
	copy(p[4:], Magic[:])
	binary.BigEndian.PutUint32(p[8:], cmd)
	return p
}

func TestCheckControlBlocksForbidden(t *testing.T) {
	for _, cmd := range []uint32{CmdSetDevInfo, CmdUpdatePsw, CmdSetWifiMode, CmdFirmUpdate, CmdXferData, CmdSetStartMode, CmdGetDevInfo} {
		if err := CheckControl(controlPacket(cmd)); err == nil {
			t.Errorf("command 0x%02X not blocked", cmd)
		}
	}
	var tok [8]byte
	if err := CheckControl(MarshalReleaseRequest(tok, 1)); err == nil {
		t.Error("RELEASE ENFORCE not blocked")
	}
	for _, p := range [][]byte{
		MarshalReleaseRequest(tok, 0),
		MarshalReserveRequest(tok, "192.168.1.2", ClientNotifyPort, "123", time.Unix(0, 0)),
		MarshalGetWifiStatusRequest(tok),
	} {
		if err := CheckControl(p); err != nil {
			t.Errorf("session command blocked: %v", err)
		}
	}
}

func TestCheckDataBlocksPixelSize(t *testing.T) {
	var tok [8]byte
	if err := CheckData(MarshalReadPixelSize(tok, 0, false)); err == nil {
		t.Fatal("PIXELSIZE not blocked")
	}
	if err := CheckData(MarshalWriteToneCurve(tok)); err == nil {
		t.Fatal("tone curve (0xDB) not blocked")
	}
	for name, p := range map[string][]byte{
		"inquiry":  MarshalGetDeviceInfo(tok),
		"params":   MarshalGetScanParams(tok),
		"settings": MarshalGetScanSettings(tok),
		"config":   MarshalConfigCommand(tok),
		"status":   MarshalGetStatus(tok),
		"prepare":  MarshalPrepareScan(tok),
		"wait":     MarshalWaitForScan(tok),
		"end":      MarshalEndScan(tok),
		"page":     MarshalPageTransfer(tok, 0, 0, true),
		"sense":    MarshalGetPageMetadata(tok),
		"scancfg":  MarshalScanConfig(tok, DefaultScanConfig()),
	} {
		if err := CheckData(p); err != nil {
			t.Errorf("%s blocked: %v", name, err)
		}
	}
}
