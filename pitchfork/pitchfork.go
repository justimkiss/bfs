package main

import (
	"os"
	"io"
	"fmt"
	"time"
	"sort"
	"errors"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"io/ioutil"
	
	log "github.com/golang/glog"
	"github.com/samuel/go-zookeeper/zk"
)

type Pitchfork struct {
	ID        string
	config    *Config
	zk        *Zookeeper
}


type PitchforkList []*Pitchfork

//Len
func (pl PitchforkList) Len() int {
	return len(pl)
}

//Less
func (pl PitchforkList) Less(i, j int) bool {
	return pl[i].ID < pl[j].ID
}

//Swap
func (pl PitchforkList) Swap(i, j int) {
	pl[i], pl[j] = pl[j], pl[i]
}

//NewPitchfork
func NewPitchfork(zk *Zookeeper, config *Config) (*Pitchfork, error) {
	var (
		id        string
		p         *Pitchfork
		err       error
	)
	if id, err = generateID(); err != nil {
		log.Errof("generateID() failed")
		return nil, err
	}

	p = &Pitchfork{ID: id, config: config, zk: zk}
	if err = p.Register(); err != nil {
		log.Errorf("Register() failed")
		return nil, err
	}

	return p, nil
}

//Register register pitchfork in the zookeeper
func (p *Pitchfork) Register() error {
	node := fmt.Sprintf("%s/%s", p.config.ZookeeperPitchforkRoot, p.ID)
	flags := int32(zk.FlagEphemeral)

	return p.zk.createPath(node, flags)
}

//WatchGetPitchforks get all the pitchfork nodes and set up the watcher in the zookeeper
func (p *Pitchfork) WatchGetPitchforks() (PitchforkList, <-chan zk.Event, error) {
	var (
		pitchforkRootPath string
		children          []string
		pitchforkChanges  <-chan zk.Event
		result            PitchforkList
		err               error
	)

	pitchforkRootPath = p.config.ZookeeperPitchforkRoot
	children, _, pitchforkChanges, err = p.zk.c.ChildrenW(pitchforkRootPath)
	if err != nil {
		log.Errorf("zk.ChildrenW(\"%s\") error(%v)", pitchforkRootPath, err)
		return nil, nil, err
	}
	
	result = make(PitchforkList, 0, len(children))
	for _, child := range children {
		pitchforkID := child
		result = append(result, &Pitchfork{ID:pitchforkID, config:p.config, zk:p.zk})
	}

	return result, pitchforkChanges, nil
}

//WatchGetStores get all the store nodes and set up the watcher in the zookeeper
func (p *Pitchfork) WatchGetStores() (StoreList, <-chan zk.Event, error) {
	var (
		storeRootPath      string
		children,children1 []string
		storeChanges       <-chan zk.Event
		result             StoreList
		data               []byte
		dataJson           map[string]interface{}
		err                error
	)

	storeRootPath = p.config.ZookeeperStoreRoot
	if _, _, storeChanges, err = p.zk.c.GetW(storeRootPath); err != nil {
		log.Errorf("zk.GetW(\"%s\") error(%v)", storeRootPath, err)
		return nil, nil, err
	}

	if children, _, err = p.zk.c.Children(storeRootPath); err != nil {
		log.Errorf("zk.Children(\"%s\") error(%v)", storeRootPath, err)
		return nil, nil, err
	}

	result = make(StoreList, 0, len(children))
	for _, child := range children {
		rackName := child
		pathRack := fmt.Sprintf("%s/%s", storeRootPath, rackName)
		if children1, _, err = p.zk.c.Children(pathRack); err != nil {
			log.Errorf("zk.Children(\"%s\") error(%v)", pathRack, err)
			return nil, nil, err
		}
		for _, child1 := range children1 {
			storeId := child1
			pathStore := fmt.Sprintf("%s/%s", pathRack, storeId)
			if data, _, err = p.zk.c.Get(pathStore); err != nil {
				log.Errorf("zk.Get(\"%s\") error(%v)", pathStore, err)
				return nil, nil, err
			}
			if err = json.Unmarshal(data, &dataJson); err != nil {
				log.Errorf("json.Marshal() error(%v)", err)
				return nil, nil, err
			}

			status := int32(dataJson["status"].(float64))
			host := string(dataJson["stat"].(string))
			result = append(result, &Store{rack:rackName, ID:storeId, host:host, status:status})
		}
	}

	return result, storeChanges, nil
}

//probeStore probe store node and feed back to directory
func (p *Pitchfork)probeStore(s *Store) error {
	var (
		status = int32(0xff)
		url      string
		body     []byte
		resp     *http.Response
		dataJson map[string]interface{}
		volumes  []interface{}
		err      error
	)
	if s.status == 0 {
		log.Warningf("probeStore() store not online host:%s", s.host)
		return nil
	}
	url = fmt.Sprintf("http://%s/info", s.host)
	if resp, err = http.Get(url); err != nil {
		status = status & 0xfc
		log.Errorf("http.Get() called error(%v)  url:%s", err, url)
		goto feedbackZk
	}

	defer resp.Body.Close()
	if body, err = ioutil.ReadAll(resp.Body); err != nil {
		log.Errorf("probeStore() ioutil.ReadAll() error(%v)", err)
		return err
	}

	if err = json.Unmarshal(body, &dataJson); err != nil {
		log.Errorf("probeStore() json.Unmarshal() error(%v)", err)
		return err
	}

	volumes =  dataJson["volumes"].([]interface{})
	for _, volume := range volumes {
		volumeValue := volume.(map[string]interface{})
		block := volumeValue["block"].(map[string]interface{})
		offset := int64(block["offset"].(float64))
		if int64(maxOffset * p.config.MaxUsedSpacePercent) < offset {
			log.Warningf("probeStore() store block has no enough space, host:%s", s.host)
			status = status & 0xfd
		}
		lastErr := block["last_err"]
		if lastErr != nil {
			status = status & 0xfc
			log.Errorf("probeStore() store error(%v) host:%s", lastErr, s.host)
			goto feedbackZk
		}
	}
	if s.status == status {
		return nil
	}

feedbackZk:
    pathStore := fmt.Sprintf("%s/%s/%s", p.config.ZookeeperStoreRoot, s.rack, s.ID)
    if err = p.zk.setStoreStatus(pathStore, status); err != nil {
    	log.Errorf("setStoreStatus() called error(%v) path:%s", err, pathStore)
    	return err
    }
    log.Infof("probeStore() called success host:%s status: %d %d", s.host, s.status, status)
    return nil
}

//Work main flow of pitchfork server
func (p *Pitchfork)Work() {
	var (
		stores             StoreList
		pitchforks         PitchforkList
		storeChanges       <-chan zk.Event
		pitchforkChanges   <-chan zk.Event
		allStores          map[string]StoreList
		stopper            chan struct{}
		store              *Store
		err                error
	)
	for {
		stores, storeChanges, err = p.WatchGetStores()
		if err != nil {
			log.Errorf("WatchGetStores() called error(%v)", err)
			return
		}

		pitchforks, pitchforkChanges, err = p.WatchGetPitchforks()
		if err != nil {
			log.Errorf("WatchGetPitchforks() called error(%v)", err)
			return
		}

		if allStores, err = divideStoreBetweenPitchfork(pitchforks, stores); err != nil {
			log.Errorf("divideStoreBetweenPitchfork() called error(%v)", err)
			return
		}

		stopper = make(chan struct{})

		for _, store = range allStores[p.ID] {
			go func(stopper chan struct{}, store *Store) {
				for {
					if err = p.probeStore(store); err != nil {
						log.Errorf("probeStore() called error(%v)", err)
					}
					select {
						case <- stopper:
							return
						case <- time.After(p.config.ProbeInterval):
					}
				}
			}(stopper, store)
		}

		select {
		case <-storeChanges:
			log.Infof("Triggering rebalance due to store list change")
			close(stopper)

		case <-pitchforkChanges:
			log.Infof("Triggering rebalance due to pitchfork list change")
			close(stopper)
		}
	}
}

//generateUUID
func generateUUID() (string, error) {
	uuid := make([]byte, 16)
	n, err := io.ReadFull(rand.Reader, uuid)
	if n != len(uuid) || err != nil {
		return "", err
	}
	uuid[8] = uuid[8]&^0xc0 | 0x80
	uuid[6] = uuid[6]&^0xf0 | 0x40
	return fmt.Sprintf("%x-%x-%x-%x-%x", uuid[0:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:]), nil
}

//generateID
func generateID() (string, error) {
	var (
		uuid      string
		hostname string
		ID       string
		err      error
	)

	uuid, err = generateUUID()
	if err != nil {
		return "", err
	}

	hostname, err = os.Hostname()
	if err != nil {
		return "", err
	}

	ID = fmt.Sprintf("%s:%s", hostname, uuid)
	return ID, nil
}

// Divides a set of stores between a set of pitchforks.
func divideStoreBetweenPitchfork(pitchforks PitchforkList, stores StoreList) (map[string]StoreList, error) {
	result := make(map[string]StoreList)

	slen := len(stores)
	plen := len(pitchforks)
	if slen == 0 || plen == 0 || slen < plen {
		return nil, errors.New("divideStoreBetweenPitchfork error")
	}

	sort.Sort(stores)
	sort.Sort(pitchforks)

	n := slen / plen
	m := slen % plen
	p := 0
	for i, pitchfork := range pitchforks {
		first := p
		last := first + n
		if m > 0 && i < m {
			last++
		}
		if last > slen {
			last = slen
		}

		for _, store := range stores[first:last] {
			result[pitchfork.ID] = append(result[pitchfork.ID], store)
		}
		p = last
	}

	return result, nil
}