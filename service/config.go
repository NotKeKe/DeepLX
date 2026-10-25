/*
 * @Author: Vincent Yang
 * @Date: 2024-04-23 00:39:03
 * @LastEditors: Jason Lyu
 * @LastEditTime: 2025-04-08 13:45:00
 * @FilePath: /DLX/service/config.go
 * @Telegram: https://t.me/missuo
 * @GitHub: https://github.com/missuo
 *
 * Copyright © 2024 by Vincent, All Rights Reserved.
 */

package service

import (
	"flag"
	"fmt"
	"os"
	"strconv"

	"github.com/OwO-Network/DLX/proxy"
)

const (
	// Refresh bounds: see proxy.ProxyPool. The list is huge, so both numbers
	// stay small by default.
	defaultProxyMaxPool          = 200
	defaultProxyProbeConcurrency = 32
)

type Config struct {
	IP                    string
	Port                  int
	Token                 string
	DlSession             string
	Proxy                 string
	ProxyListURL          string
	UpdateInterval        int
	LatencyTimeout        int
	ProxyMaxPool          int
	ProxyProbeConcurrency int
	ProxyPool             *proxy.ProxyPool
}

func InitConfig() *Config {
	cfg := &Config{
		IP:                    "0.0.0.0",
		Port:                  1188,
		ProxyMaxPool:          defaultProxyMaxPool,
		ProxyProbeConcurrency: defaultProxyProbeConcurrency,
	}

	// IP flag
	if ip, ok := os.LookupEnv("IP"); ok && ip != "" {
		cfg.IP = ip
	}
	flag.StringVar(&cfg.IP, "ip", cfg.IP, "set up the IP address to bind to")
	flag.StringVar(&cfg.IP, "i", cfg.IP, "set up the IP address to bind to")

	// Port flag
	if port, ok := os.LookupEnv("PORT"); ok && port != "" {
		fmt.Sscanf(port, "%d", &cfg.Port)
	}
	flag.IntVar(&cfg.Port, "port", cfg.Port, "set up the port to listen on")
	flag.IntVar(&cfg.Port, "p", cfg.Port, "set up the port to listen on")

	// DL Session flag
	flag.StringVar(&cfg.DlSession, "s", "", "set the dl-session for /v1/translate endpoint")
	if cfg.DlSession == "" {
		if dlSession, ok := os.LookupEnv("DL_SESSION"); ok {
			cfg.DlSession = dlSession
		}
	}

	// Access token flag
	flag.StringVar(&cfg.Token, "token", "", "set the access token for /translate endpoint")
	if cfg.Token == "" {
		if token, ok := os.LookupEnv("TOKEN"); ok {
			cfg.Token = token
		}
	}

	// HTTP Proxy flag
	flag.StringVar(&cfg.Proxy, "proxy", "", "set the proxy URL for HTTP requests")
	if cfg.Proxy == "" {
		if proxy, ok := os.LookupEnv("PROXY"); ok {
			cfg.Proxy = proxy
		}
	}

	// Proxy List URL flag
	flag.StringVar(&cfg.ProxyListURL, "proxy-list-url", "", "set the URL to fetch proxy list from")
	if cfg.ProxyListURL == "" {
		if proxyListURL, ok := os.LookupEnv("PROXY_LIST_URL"); ok {
			cfg.ProxyListURL = proxyListURL
		}
	}

	// Proxy Update Interval flag
	if updateInterval, ok := os.LookupEnv("PROXY_UPDATE_INTERVAL"); ok && updateInterval != "" {
		fmt.Sscanf(updateInterval, "%d", &cfg.UpdateInterval)
	} else {
		cfg.UpdateInterval = 5
	}
	flag.IntVar(&cfg.UpdateInterval, "proxy-update-interval", cfg.UpdateInterval, "set the interval in minutes to update proxy list")

	// Latency Timeout flag
	if latencyTimeout, ok := os.LookupEnv("PROXY_LATENCY_TIMEOUT"); ok && latencyTimeout != "" {
		fmt.Sscanf(latencyTimeout, "%d", &cfg.LatencyTimeout)
	} else {
		cfg.LatencyTimeout = 3
	}
	flag.IntVar(&cfg.LatencyTimeout, "proxy-latency-timeout", cfg.LatencyTimeout, "set the latency timeout in seconds")

	// Proxy Max Pool flag: how many list entries one refresh probes and keeps.
	cfg.ProxyMaxPool = envPositiveInt("PROXY_MAX_POOL", defaultProxyMaxPool)
	flag.IntVar(&cfg.ProxyMaxPool, "proxy-max-pool", cfg.ProxyMaxPool, "set the maximum number of proxies to probe and keep per refresh")

	// Proxy Probe Concurrency flag: simultaneous probes per refresh.
	cfg.ProxyProbeConcurrency = envPositiveInt("PROXY_PROBE_CONCURRENCY", defaultProxyProbeConcurrency)
	flag.IntVar(&cfg.ProxyProbeConcurrency, "proxy-probe-concurrency", cfg.ProxyProbeConcurrency, "set how many proxies to probe concurrently")

	flag.Parse()

	// Clamp a non-positive flag value back to the safe defaults.
	if cfg.ProxyMaxPool < 1 {
		cfg.ProxyMaxPool = defaultProxyMaxPool
	}
	if cfg.ProxyProbeConcurrency < 1 {
		cfg.ProxyProbeConcurrency = defaultProxyProbeConcurrency
	}

	return cfg
}

// envPositiveInt reads a positive integer from the environment and falls back
// to fallback when the variable is unset, empty, or not a positive integer.
func envPositiveInt(name string, fallback int) int {
	value, ok := os.LookupEnv(name)
	if !ok || value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		return fallback
	}
	return parsed
}
