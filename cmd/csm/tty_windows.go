package main

func isTerminal(uintptr) bool { return false }

func setEcho(uintptr, bool) error { return errWindows }
