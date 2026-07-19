package main

import (
	"example.com/demo/pkga"
	"example.com/replaced"
)

func Run() string {
	return pkga.Helper() + replaced.Replaced()
}
