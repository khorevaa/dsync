/*
 * Minio Cloud Storage, (C) 2016 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package dsync

import (
	"log"
	"math"
	"math/rand"
	"net/rpc"
	"sync"
	"time"
)

const DMutexAcquireTimeout = 25 * time.Millisecond

// A DMutex is a distributed mutual exclusion lock.
type DMutex struct {
	Name  string
	locks []bool     // Array of nodes that granted a lock
	m     sync.Mutex // Mutex to prevent multiple simultaneous locks from this node

	// TODO: Decide: create per object or create once for whole class
	clnts []*rpc.Client
}

type Granted struct {
	index  int
	locked bool
}

func connectLazy(dm *DMutex) {
	if dm.clnts == nil {
		dm.clnts = make([]*rpc.Client, n)
	}
	for i := range dm.clnts {
		if dm.clnts[i] != nil {
			continue
		}
		dm.clnts[i], _ = rpc.DialHTTPPath("tcp", nodes[i], rpcPath)
	}
}

// Lock locks dm.
//
// If the lock is already in use, the calling goroutine
// blocks until the mutex is available.
func (dm *DMutex) Lock() {

	// Shield Lock() with local mutex in order to prevent more than
	// one broadcast going out at the same time from this node
	dm.m.Lock()
	defer dm.m.Unlock()

	runs, backOff := 1, 1

	for {
		connectLazy(dm)
		locks := make([]bool, n)
		success := lock(dm.clnts, &locks, dm.Name)
		if success {
			dm.locks = make([]bool, n)
			copy(dm.locks, locks[:])
			return
		}

		// We timed out on the previous lock, incrementally wait for a longer back-off time,
		// and try again afterwards
		time.Sleep(time.Duration(backOff) * time.Millisecond)

		backOff += int(rand.Float64() * math.Pow(2, float64(runs)))
		if backOff > 1024 {
			backOff = backOff % 64

			runs = 1 // reset runs
		} else if runs < 10 {
			runs++
		}

		//fmt.Println(backOff)
	}
}

// lock tries to acquire the distributed lock, returning true or false
//
func lock(clnts []*rpc.Client, locks *[]bool, lockName string) bool {

	// Create buffered channel of quorum size
	ch := make(chan Granted, n/2+1)

	for index, c := range clnts {

		// broadcast lock request to all nodes
		go func(index int, c *rpc.Client) {
			// All client methods issuing RPCs are thread-safe and goroutine-safe,
			// i.e. it is safe to call them from multiple concurrently running go routines.
			var status bool
			err := c.Call("Dsync.Lock", lockName, &status)

			locked := false
			if err == nil {
				locked = status
			} else {
				// silently ignore error, retry later
			}

			ch <- Granted{index: index, locked: locked}

		}(index, c)
	}

	var wg sync.WaitGroup
	wg.Add(1)

	quorum := false

	go func() {

		// Wait until we have received (minimally) quorum number of responses or timeout
		i := 0
		done := false
		timeout := time.After(DMutexAcquireTimeout)

		for ; i < n; i++ {

			select {
			case grant := <-ch:
				if grant.locked {
					// Mark that this node has acquired the lock
					(*locks)[grant.index] = true
				} else {
					done = true
					//fmt.Println("one lock failed before quorum -- release locks acquired")
					releaseAll(clnts, locks, lockName)
				}

			case <-timeout:
				done = true
				// timeout happened, maybe one of the nodes is slow, count
				// number of locks to check whether we have quorum or not
				if !quorumMet(locks) {
					//fmt.Println("timed out -- release locks acquired")
					releaseAll(clnts, locks, lockName)
				}
			}

			if done {
				break
			}
		}

		// Count locks in order to determine whterh we have quorum or not
		quorum = quorumMet(locks)

		// Signal that we have the quorum
		wg.Done()

		// Wait for the other responses and immediately release the locks
		// (do not add them to the locks array because the DMutex could
		//  already has been unlocked again by the original calling thread)
		for ; i < n; i++ {
			grantToBeReleased := <-ch
			if grantToBeReleased.locked {
				// release lock
				go sendRelease(clnts[grantToBeReleased.index], lockName)
			}
		}
	}()

	wg.Wait()

	return quorum
}

// quorumMet determines whether we have acquired n/2+1 underlying locks or not
func quorumMet(locks *[]bool) bool {

	count := 0
	for _, locked := range *locks {
		if locked {
			count++
		}
	}

	return count >= n/2+1
}

// releaseAll releases all locks that are marked as locked
func releaseAll(clnts []*rpc.Client, locks *[]bool, lockName string) {

	for lock := 0; lock < n; lock++ {
		if (*locks)[lock] {
			go sendRelease(clnts[lock], lockName)
			(*locks)[lock] = false
		}
	}

}

// hasLock returns whether or not a node participated in granting the lock
func (dm *DMutex) hasLock(node string) bool {

	for index, n := range nodes {
		if n == node {
			return dm.locks[index]
		}
	}

	return false
}

// locked returns whether or not we have met the quorum
func (dm *DMutex) locked() bool {

	locks := make([]bool, n)
	copy(locks[:], dm.locks[:])

	return quorumMet(&locks)
}

// Unlock unlocks dm.
//
// It is a run-time error if dm is not locked on entry to Unlock.
func (dm *DMutex) Unlock() {

	// Verify that we have the lock or panic otherwise (similar to sync.mutex)
	if !dm.locked() {
		panic("dsync: unlock of unlocked distributed mutex")
	}

	// We don't need to wait until we have released all the locks (or the quorum)
	// (a subsequent lock will retry automatically in case it would fail to get
	//  quorum)
	for index, c := range dm.clnts {

		if dm.locks[index] {
			// broadcast lock release to all nodes the granted the lock
			go sendRelease(c, dm.Name)

			dm.locks[index] = false
		}
	}
}

// sendRelease sends a release message to a node that previously granted a lock
func sendRelease(c *rpc.Client, name string) {

	// All client methods issuing RPCs are thread-safe and goroutine-safe,
	// i.e. it is safe to call them from multiple concurrently running goroutines.
	var status bool
	if err := c.Call("Dsync.Unlock", name, &status); err != nil {
		log.Fatal("Unlock on %s failed on client %v", name, c)
	}
}
