package scrapers

import (
	"errors"
	"fmt"
	"github.com/diadata-org/diadata/pkg/dia"
	"github.com/diadata-org/diadata/pkg/dia/helpers"
	ws "github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
	"hash/fnv"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

var _LBankSocketurl string = "wss://api.lbkex.com/ws/V2/"

type ResponseLBank struct {
	Pair   string      `json:"pair"`
	Trade  interface{} `json:"trade"`
	Type   string      `json:"type"`
	Server string      `json:"SERVER"`
	Ts     string      `json:"TS"`
}

type SubscribeLBank struct {
	Action    string `json:"action"`
	Subscribe string `json:"subscribe"`
	Pair      string `json:"pair"`
}

type LBankScraper struct {
	wsClient *ws.Conn
	// signaling channels for session initialization and finishing
	shutdown     chan nothing
	shutdownDone chan nothing
	// error handling; to read error or closed, first acquire read lock
	// only cleanup method should hold write lock
	errorLock sync.RWMutex
	error     error
	closed    bool
	// used to keep track of trading pairs that we subscribed to
	pairScrapers map[string]*LBankPairScraper
	exchangeName string
}

// NewLBankScraper returns a new LBankScraper for the given pair
func NewLBankScraper(exchangeName string) *LBankScraper {

	s := &LBankScraper{
		shutdown:     make(chan nothing),
		shutdownDone: make(chan nothing),
		pairScrapers: make(map[string]*LBankPairScraper),
		exchangeName: exchangeName,
		error:        nil,
	}

	var wsDialer ws.Dialer
	SwConn, _, err := wsDialer.Dial(_LBankSocketurl, nil)

	if err != nil {
		println(err.Error())
	}

	s.wsClient = SwConn
	go s.mainLoop()
	return s
}

// runs in a goroutine until s is closed
func (s *LBankScraper) mainLoop() {
	var err error

	for true {
		message := &ResponseLBank{}
		if err = s.wsClient.ReadJSON(&message); err != nil {
			println(err.Error())
			break
		}
		ps, ok := s.pairScrapers[strings.ToUpper(message.Pair)]

		if ok {
			var f64Price float64
			var f64Volume float64

			switch message.Trade.(type) {
			case []interface{}:
				md := message.Trade.([]interface{})
				f64Price = md[1].(float64)
				f64Volume = md[2].(float64)
				if md[3] == "sell" {
					f64Volume = -f64Volume
				}
			case map[string]interface{}:
				md := message.Trade.(map[string]interface{})
				f64Price = md["price"].(float64)
				f64Volume = md["volume"].(float64)
				if md["direction"] == "sell" {
					f64Volume = -f64Volume
				}
			}

			timeStamp := time.Now().UTC()
			t := &dia.Trade{
				Symbol:         ps.pair.Symbol,
				Pair:           strings.ToUpper(message.Pair),
				Price:          f64Price,
				Volume:         f64Volume,
				Time:           timeStamp,
				ForeignTradeID: strconv.FormatInt(int64(hash(timeStamp.String())), 16),
				Source:         s.exchangeName,
			}
			ps.chanTrades <- t
		}
	}
	s.cleanup(err)
}

func hash(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}

func (s *LBankScraper) cleanup(err error) {
	s.errorLock.Lock()
	defer s.errorLock.Unlock()

	if err != nil {
		s.error = err
	}
	s.closed = true

	close(s.shutdownDone)
}

// Close closes any existing API connections, as well as channels of
// PairScrapers from calls to ScrapePair
func (s *LBankScraper) Close() error {

	if s.closed {
		return errors.New("LBankScraper: Already closed")
	}
	s.wsClient.Close()
	close(s.shutdown)
	<-s.shutdownDone
	s.errorLock.RLock()
	defer s.errorLock.RUnlock()
	return s.error
}

// ScrapePair returns a PairScraper that can be used to get trades for a single pair from
// this APIScraper
func (s *LBankScraper) ScrapePair(pair dia.Pair) (PairScraper, error) {
	s.errorLock.RLock()
	defer s.errorLock.RUnlock()
	if s.error != nil {
		return nil, s.error
	}
	if s.closed {
		return nil, errors.New("LBankScraper: Call ScrapePair on closed scraper")
	}
	ps := &LBankPairScraper{
		parent:     s,
		pair:       pair,
		chanTrades: make(chan *dia.Trade),
	}
	s.pairScrapers[pair.ForeignName] = ps
	a := &SubscribeLBank{
		Action:    "subscribe",
		Subscribe: "trade",
		Pair:      strings.ToLower(pair.ForeignName),
	}
	if err := s.wsClient.WriteJSON(a); err != nil {
		fmt.Println(err.Error())
	}
	return ps, nil
}

func (s *LBankScraper) normalizeSymbol(foreignName string) (symbol string, err error) {
	str := strings.Split(foreignName, "_")
	symbol = strings.ToUpper(str[0])
	if helpers.NameForSymbol(symbol) == symbol {
		if !helpers.SymbolIsName(symbol) {
			return symbol, errors.New("Foreign name can not be normalized:" + foreignName + " symbol:" + symbol)
		}
	}
	if helpers.SymbolIsBlackListed(symbol) {
		return symbol, errors.New("Symbol is black listed:" + symbol)
	}
	return symbol, nil
}

// FetchAvailablePairs returns a list with all available trade pairs
func (s *LBankScraper) FetchAvailablePairs() (pairs []dia.Pair, err error) {
	response, err := http.Get("https://api.lbkex.com/v1/currencyPairs.do")
	if err != nil {
		log.Error("The HTTP request failed:", err)
		return
	}
	defer response.Body.Close()
	data, _ := ioutil.ReadAll(response.Body)
	ls := strings.Split(strings.Replace(string(data)[1:len(data)-1], "\"", "", -1), ",")
	for _, p := range ls {
		symbol, serr := s.normalizeSymbol(p)
		if serr == nil {
			pairs = append(pairs, dia.Pair{
				Symbol:      symbol,
				ForeignName: p,
				Exchange:    s.exchangeName,
			})
		} else {
			log.Error(serr)
		}
	}
	return
}

// LBankPairScraper implements PairScraper for LBank exchange
type LBankPairScraper struct {
	parent     *LBankScraper
	pair       dia.Pair
	chanTrades chan *dia.Trade
	closed     bool
}

// Close stops listening for trades of the pair associated with s
func (ps *LBankPairScraper) Close() error {
	return nil
}

// Channel returns a channel that can be used to receive trades
func (ps *LBankPairScraper) Channel() chan *dia.Trade {
	return ps.chanTrades
}

// Error returns an error when the channel Channel() is closed
// and nil otherwise
func (ps *LBankPairScraper) Error() error {
	s := ps.parent
	s.errorLock.RLock()
	defer s.errorLock.RUnlock()
	return s.error
}

// Pair returns the pair this scraper is subscribed to
func (ps *LBankPairScraper) Pair() dia.Pair {
	return ps.pair
}
