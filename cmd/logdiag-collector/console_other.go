// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !windows

package main

import "os"

func ownsConsole() bool { return false }

func usable(*os.File) bool { return true }

func prepareConsole(consoleNeed) bool { return false }

func alert(string) {}
