// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package main

// protectSelf does nothing outside Linux; see the README's limits.
func protectSelf() error { return nil }
