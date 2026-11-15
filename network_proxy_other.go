//go:build !windows

package main

import "net/url"

func systemNetworkProxy(*url.URL) (*url.URL, error) { return nil, nil }
