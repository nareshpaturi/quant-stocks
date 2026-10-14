package domain

import "time"

// MarketClock is the broker's view of the regular US equity session right now.
// NextClose is today's close while the market is open.
type MarketClock struct {
	Timestamp time.Time
	IsOpen    bool
	NextOpen  time.Time
	NextClose time.Time
}

// MarketSession is one trading day's regular session from the exchange
// calendar. Early-close days carry their shortened Close.
type MarketSession struct {
	Date  string // YYYY-MM-DD in New York
	Open  time.Time
	Close time.Time
}

// Quote is the latest top-of-book and last-trade price for one symbol. Any
// field may be zero when the data feed has nothing recent.
type Quote struct {
	Bid  float64
	Ask  float64
	Last float64
}
