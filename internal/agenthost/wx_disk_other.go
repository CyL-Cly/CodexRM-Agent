//go:build !linux

package agenthost

func wxDiskFree(string) uint64 { return 0 }
