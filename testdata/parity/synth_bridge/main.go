package main

/*
#include <stdio.h>
void c_hello(void);
*/
import "C"

func CallC() {
	C.c_hello()
}

func main() {}
