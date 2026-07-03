package main

import (
	"fmt"
	"time"

	"github.com/tupitsin88/ratelimiter"
)

func main() {
	l := ratelimiter.New(5, time.Second)

	for i := 1; i <= 8; i++ {
		fmt.Printf("запрос %d: %v\n", i, l.Allow("user1"))
	}
}
