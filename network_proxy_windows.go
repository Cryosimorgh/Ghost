package main

import (
	"encoding/binary"
	"net/url"
	"syscall"
)

func systemNetworkProxy(target *url.URL) (*url.URL, error) {
	path, _ := syscall.UTF16PtrFromString(`Software\Microsoft\Windows\CurrentVersion\Internet Settings`)
	var key syscall.Handle
	if err := syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, path, 0, syscall.KEY_QUERY_VALUE, &key); err != nil {
		return nil, nil
	}
	defer syscall.RegCloseKey(key)
	read := func(name string) []byte {
		field, _ := syscall.UTF16PtrFromString(name)
		var kind, size uint32
		if syscall.RegQueryValueEx(key, field, nil, &kind, nil, &size) != nil || size == 0 || size > 32768 {
			return nil
		}
		data := make([]byte, size)
		if syscall.RegQueryValueEx(key, field, nil, &kind, &data[0], &size) != nil {
			return nil
		}
		return data[:size]
	}
	enabled := read("ProxyEnable")
	if len(enabled) < 4 || binary.LittleEndian.Uint32(enabled) == 0 {
		return nil, nil
	}
	text := func(data []byte) string {
		words := make([]uint16, len(data)/2)
		for i := range words {
			words[i] = binary.LittleEndian.Uint16(data[i*2:])
		}
		return syscall.UTF16ToString(words)
	}
	return parseSystemProxy(text(read("ProxyServer")), text(read("ProxyOverride")), target)
}
