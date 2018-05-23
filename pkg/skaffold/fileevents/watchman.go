package fileevents

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

type SubscribeConfirmEvent struct {
	Version   string `json:"version"`
	Subscribe string `json:"subscribe"`
}

type SubscribeRequestEvent struct {
	Name     string
	Root     string
	Criteria interface{}
	Notify   chan error
	Fields   []string
}

type ErrorEvent struct {
	Error string `json:"error"`
}

type Event struct {
	Version      string `json:"version"`
	Subscription string `json:"subscription"`
	Clock        string `json:"clock"`
	Files        []File `json:"files"`
	Root         string `json:"root"`
}

type File struct {
	Name           string `json:"name"`
	New            bool   `json:"new"`
	Exists         bool   `json:"exists"`
	ContentSHA1Hex string `json:"content.sha1hex"`
}

func fileFields() []string {
	return []string{"name", "exists", "new", "content.sha1hex"}
}

type Subscription struct {
	Events                 <-chan *Event
	subscribeRequestOutbox chan<- *SubscribeRequestEvent
	closed                 bool
	closer                 io.Closer
	root                   string
	name                   string
}

type EventContentChangeFilter struct {
	digestByPath      map[string]string
	includeFirstEvent bool
}

func NewEventContentChangeFilter() *EventContentChangeFilter {
	return &EventContentChangeFilter{
		includeFirstEvent: false,
		digestByPath:      map[string]string{},
	}
}

func (e *EventContentChangeFilter) FilterContentChanges(incomingEvents <-chan *Event) <-chan *Event {
	filteredEvents := make(chan *Event)

	go func() {
		defer close(filteredEvents)

		for event := range incomingEvents {
			changedFiles := make([]File, 0, len(event.Files))
			for _, file := range event.Files {
				path := filepath.Join(event.Root, file.Name)

				prevDigest, wasPresent := e.digestByPath[path]

				if file.Exists {
					digest := file.ContentSHA1Hex
					if digest != prevDigest {
						if !wasPresent {
							file.New = true
						} else {
							file.New = false
						}
						e.digestByPath[path] = digest
						if wasPresent || e.includeFirstEvent {
							changedFiles = append(changedFiles, file)
						}
					}
				}

				// Since we might experience a race above, reevaluate instead of using else
				if !file.Exists {
					if wasPresent {
						delete(e.digestByPath, path)
						changedFiles = append(changedFiles, file)
					}
				}
			}

			if len(changedFiles) > 0 {
				event.Files = changedFiles
				log.Println("Emitting", *event)
				filteredEvents <- event
			}
		}
	}()

	return filteredEvents
}

func newSubscription(conn *WatchmanConn, root, name string) *Subscription {
	return &Subscription{
		Events:                 conn.EventInbox,
		subscribeRequestOutbox: conn.SubscribeRequestOutbox,
		closer:                 conn.Closer,
		root:                   root,
		name:                   name,
	}
}

func (s *Subscription) Update(expr interface{}) error {
	notify := make(chan error, 1)
	req := &SubscribeRequestEvent{
		Root: s.root,
		Name: s.name,
		Criteria: map[string]interface{}{
			"expression": expr,
			"fields":     fileFields(),
		},
		Notify: notify,
	}
	s.subscribeRequestOutbox <- req

	err := <-notify

	if err != nil {
		return err
	}

	return nil
}

func (s *Subscription) Close() error {
	if s.closed {
		return nil
	}

	s.closed = true
	close(s.subscribeRequestOutbox)
	return s.closer.Close()
}

type Watcher interface {
	Subscribe(root string, name string, expr interface{}) (*Subscription, error)
}

type WatcherCloser interface {
	Watcher
	Close() error
}

func tolerantDial(sockname string) (*net.UnixConn, error) {
	for {
		conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: sockname, Net: "unix"})
		if err == nil {
			return conn, err
		}

		opErr, ok := err.(*net.OpError)
		if !ok {
			return conn, err
		}

		syscallError, ok := opErr.Err.(*os.SyscallError)
		if !ok {
			return conn, err
		}

		switch syscallError.Err {
		case syscall.ECONNREFUSED:
			time.Sleep(50 * time.Millisecond)
			continue
		case syscall.ENOENT:
			time.Sleep(50 * time.Millisecond)
			continue
		case syscall.ENODATA:
			time.Sleep(50 * time.Millisecond)
			continue
		}

		fmt.Fprintf(os.Stderr, "Saw error %#v\n", syscallError)
		return conn, err
	}
}

func UseWatchman() (Watcher, error) {
	out, err := exec.Command("watchman", "get-sockname").Output()
	if err != nil {
		return nil, err
	}

	getSockname := struct {
		Sockname string `json:"sockname"`
	}{}
	err = json.Unmarshal(out, &getSockname)
	if err != nil {
		return nil, err
	}

	return &WatchmanService{sockname: getSockname.Sockname}, nil
}

func StartWatchman(sockname string) (WatcherCloser, error) {
	cmd := exec.Command(
		"watchman",
		"--no-save-state",
		"--foreground",
		"--pidfile", sockname+".pid",
		"--sockname", sockname,
		"--logfile", "/dev/stdout")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Start()
	if err != nil {
		return nil, err
	}

	return &privateWatchmanService{
		process:         cmd.Process,
		WatchmanService: WatchmanService{sockname: sockname},
	}, nil
}

func (w *WatchmanService) Subscribe(root string, name string, expr interface{}) (*Subscription, error) {
	root, err := realpath(root)
	if err != nil {
		return nil, err
	}

	conn, err := w.Connect()
	if err != nil {
		return nil, err
	}

	sub := newSubscription(conn, root, name)
	sub.Update(expr)

	return sub, nil
}

type WatchmanService struct {
	sockname string
}

type privateWatchmanService struct {
	process *os.Process
	WatchmanService
}

func (w *privateWatchmanService) Close() error {
	err := w.process.Kill()
	if err != nil {
		_, err = w.process.Wait()
	}

	return err
}

type WatchmanConn struct {
	EventInbox             <-chan *Event
	SubscribeConfirmInbox  <-chan *SubscribeConfirmEvent
	SubscribeRequestOutbox chan<- *SubscribeRequestEvent
	Closer                 io.Closer
}

func (w *WatchmanService) Connect() (*WatchmanConn, error) {
	// Open unix socket and keep it open for the future
	conn, err := tolerantDial(w.sockname)
	if err != nil {
		return nil, err
	}

	subscribeConfirmChan := make(chan *SubscribeConfirmEvent, 1)
	subscribeRequestChan := make(chan *SubscribeRequestEvent)
	encoder := json.NewEncoder(conn)

	go func(confirmChan chan *SubscribeConfirmEvent,
		requestChan chan *SubscribeRequestEvent) {
		defer conn.CloseRead()

		awaiting := map[string]*SubscribeRequestEvent{}
	Loop:
		for {
			select {
			case s, ok := <-requestChan:
				if !ok {
					requestChan = nil
					break Loop
				}

				awaiting[s.Name] = s
				message := []interface{}{"subscribe", s.Root, s.Name, s.Criteria}
				err = encoder.Encode(message)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error encoding message %#v: %#v\n", message, err.Error())
				}
			case subscribeConfirm, ok := <-confirmChan:
				if !ok {
					confirmChan = nil
					break Loop
				}
				s, ok := awaiting[subscribeConfirm.Subscribe]
				if ok {
					delete(awaiting, subscribeConfirm.Subscribe)
					close(s.Notify)
				}
			}
		}

		for _, s := range awaiting {
			s.Notify <- fmt.Errorf("Subscription closed without acknowledging subscription")
			close(s.Notify)
		}

		if requestChan != nil {
			for _ = range requestChan {
				// Drain requestChan...
			}
		}
	}(subscribeConfirmChan, subscribeRequestChan)

	eventChan := make(chan *Event, 1)
	decoder := json.NewDecoder(conn)
	go func() {
		defer close(eventChan)
		defer close(subscribeConfirmChan)
		defer conn.CloseWrite()

		for {
			var raw json.RawMessage
			err := decoder.Decode(&raw)
			if err == io.EOF {
				break
			}

			if err != nil {
				_, ok := err.(*net.OpError)
				if ok {
					break
				}

				fmt.Fprintf(os.Stderr, "Error decoding message: %#v\n", err.Error())
				break
			}

			var errEv ErrorEvent
			if err = json.Unmarshal(raw, &errEv); err != nil {
				fmt.Fprintf(os.Stderr, "Error decoding event: %#v\n", err.Error())
				break
			}

			if errEv.Error != "" {
				fmt.Fprintf(os.Stderr, "Watchman service sent error: %s\n", errEv.Error)
				continue
			}

			var event Event
			if err = json.Unmarshal(raw, &event); err != nil {
				fmt.Fprintf(os.Stderr, "Error decoding event: %#v\n", err.Error())
				break
			}

			if event.Subscription == "" {
				var subEv SubscribeConfirmEvent
				if err := json.Unmarshal(raw, &subEv); err != nil {
					fmt.Fprintf(os.Stderr, "Error decoding subscribe: %#v\n", err.Error())
					break
				}
				subscribeConfirmChan <- &subEv
			} else {
				eventChan <- &event
			}
		}
	}()

	return &WatchmanConn{
		EventInbox:             eventChan,
		SubscribeConfirmInbox:  subscribeConfirmChan,
		SubscribeRequestOutbox: subscribeRequestChan,
		Closer:                 conn,
	}, nil
}
