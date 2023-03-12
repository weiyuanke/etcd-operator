/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package etcd

import (
	"context"
	"crypto/tls"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	"go.etcd.io/etcd/client/pkg/v3/transport"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
)

const etcdTimeout = 2 * time.Second

// Exponential backoff for etcd operations (up to ~200 seconds)
var etcdBackoff = wait.Backoff{
	Steps:    18,
	Duration: 100 * time.Millisecond,
	Factor:   1.5,
	Jitter:   0.1,
}

// ClusterInterrogator is an interface to get etcd cluster related information
type ClusterInterrogator interface {
	CheckClusterHealth() error
	WaitForClusterAvailable(retries int, retryInterval time.Duration) (bool, error)
	Sync() error
	ListMembers() ([]Member, error)
	AddMember(name string, peerAddrs string) ([]Member, error)
	AddMemberAsLearner(name string, peerAddrs string) ([]Member, error)
	MemberPromote(learnerID uint64) error
	GetMemberID(peerURL string) (uint64, error)
	RemoveMember(id uint64) ([]Member, error)
}

// Client provides connection parameters for an etcd cluster
type Client struct {
	Endpoints []string
	TLS       *tls.Config
}

// New creates a new EtcdCluster client
func NewClient(endpoints []string, ca, cert, key string) (*Client, error) {
	client := Client{Endpoints: endpoints}

	if ca != "" || cert != "" || key != "" {
		tlsInfo := transport.TLSInfo{
			CertFile:      cert,
			KeyFile:       key,
			TrustedCAFile: ca,
		}
		tlsConfig, err := tlsInfo.ClientConfig()
		if err != nil {
			return nil, err
		}
		client.TLS = tlsConfig
	}

	// synchronizes client's endpoints with the known endpoints from the etcd membership.
	err := client.Sync()
	if err != nil {
		return nil, errors.Wrap(err, "error syncing endpoints with etcd")
	}
	klog.V(1).Infof("update etcd endpoints: %s", strings.Join(client.Endpoints, ","))

	return &client, nil
}

// Sync synchronizes client's endpoints with the known endpoints from the etcd membership.
func (c *Client) Sync() error {
	// Syncs the list of endpoints
	var cli *clientv3.Client
	var lastError error
	err := wait.ExponentialBackoff(etcdBackoff, func() (bool, error) {
		var err error
		cli, err = clientv3.New(clientv3.Config{
			Endpoints:   c.Endpoints,
			DialTimeout: etcdTimeout,
			DialOptions: []grpc.DialOption{
				grpc.WithBlock(), // block until the underlying connection is up
			},
			TLS: c.TLS,
		})
		if err != nil {
			lastError = err
			return false, nil
		}
		defer cli.Close()

		ctx, cancel := context.WithTimeout(context.Background(), etcdTimeout)
		err = cli.Sync(ctx)
		cancel()
		if err == nil {
			return true, nil
		}
		klog.V(5).Infof("Failed to sync etcd endpoints: %v", err)
		lastError = err
		return false, nil
	})
	if err != nil {
		return lastError
	}
	klog.V(1).Infof("etcd endpoints read from etcd: %s", strings.Join(cli.Endpoints(), ","))

	c.Endpoints = cli.Endpoints()
	return nil
}

// Member struct defines an etcd member; it is used for avoiding to spread github.com/coreos/etcd dependency
// across kubeadm codebase
type Member struct {
	Name    string
	PeerURL string
}

func (c *Client) listMembers() (*clientv3.MemberListResponse, error) {
	// Gets the member list
	var lastError error
	var resp *clientv3.MemberListResponse
	err := wait.ExponentialBackoff(etcdBackoff, func() (bool, error) {
		cli, err := clientv3.New(clientv3.Config{
			Endpoints:   c.Endpoints,
			DialTimeout: etcdTimeout,
			DialOptions: []grpc.DialOption{
				grpc.WithBlock(), // block until the underlying connection is up
			},
			TLS: c.TLS,
		})
		if err != nil {
			lastError = err
			return false, nil
		}
		defer cli.Close()

		ctx, cancel := context.WithTimeout(context.Background(), etcdTimeout)
		resp, err = cli.MemberList(ctx)
		cancel()
		if err == nil {
			return true, nil
		}
		klog.V(5).Infof("Failed to get etcd member list: %v", err)
		lastError = err
		return false, nil
	})
	if err != nil {
		return nil, lastError
	}
	return resp, nil
}

// GetMemberID returns the member ID of the given peer URL
func (c *Client) GetMemberID(peerURL string) (uint64, error) {
	resp, err := c.listMembers()
	if err != nil {
		return 0, err
	}

	for _, member := range resp.Members {
		if member.GetPeerURLs()[0] == peerURL {
			return member.GetID(), nil
		}
	}
	return 0, nil
}

// ListMembers returns the member list.
func (c *Client) ListMembers() ([]Member, error) {
	resp, err := c.listMembers()
	if err != nil {
		return nil, err
	}

	ret := make([]Member, 0, len(resp.Members))
	for _, m := range resp.Members {
		ret = append(ret, Member{Name: m.Name, PeerURL: m.PeerURLs[0]})
	}
	return ret, nil
}

// RemoveMember notifies an etcd cluster to remove an existing member
func (c *Client) RemoveMember(id uint64) ([]Member, error) {
	// Remove an existing member from the cluster
	var lastError error
	var resp *clientv3.MemberRemoveResponse
	err := wait.ExponentialBackoff(etcdBackoff, func() (bool, error) {
		cli, err := clientv3.New(clientv3.Config{
			Endpoints:   c.Endpoints,
			DialTimeout: etcdTimeout,
			DialOptions: []grpc.DialOption{
				grpc.WithBlock(), // block until the underlying connection is up
			},
			TLS: c.TLS,
		})
		if err != nil {
			lastError = err
			return false, nil
		}
		defer cli.Close()

		ctx, cancel := context.WithTimeout(context.Background(), etcdTimeout)
		resp, err = cli.MemberRemove(ctx, id)
		cancel()
		if err == nil {
			return true, nil
		}
		klog.V(5).Infof("Failed to remove etcd member: %v", err)
		lastError = err
		return false, nil
	})
	if err != nil {
		return nil, lastError
	}

	// Returns the updated list of etcd members
	ret := []Member{}
	for _, m := range resp.Members {
		ret = append(ret, Member{Name: m.Name, PeerURL: m.PeerURLs[0]})
	}

	return ret, nil
}

// AddMember adds a new member into the etcd cluster
func (c *Client) AddMember(name string, peerAddrs string) ([]Member, error) {
	return c.addMember(name, peerAddrs, false)
}

// AddMemberAsLearner adds a new learner member into the etcd cluster.
func (c *Client) AddMemberAsLearner(name string, peerAddrs string) ([]Member, error) {
	return c.addMember(name, peerAddrs, true)
}

// addMember notifies an existing etcd cluster that a new member is joining, and
// return the updated list of members. If the member has already been added to the
// cluster, this will return the existing list of etcd members.
func (c *Client) addMember(name string, peerAddrs string, isLearner bool) ([]Member, error) {
	// Parse the peer address, required to add the client URL later to the list
	// of endpoints for this client. Parsing as a first operation to make sure that
	// if this fails no member addition is performed on the etcd cluster.
	parsedPeerAddrs, err := url.Parse(peerAddrs)
	if err != nil {
		return nil, errors.Wrapf(err, "error parsing peer address %s", peerAddrs)
	}

	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   c.Endpoints,
		DialTimeout: etcdTimeout,
		DialOptions: []grpc.DialOption{
			grpc.WithBlock(), // block until the underlying connection is up
		},
		TLS: c.TLS,
	})
	if err != nil {
		return nil, err
	}
	defer cli.Close()

	// Adds a new member to the cluster
	var (
		lastError   error
		respMembers []*etcdserverpb.Member
		learnerID   uint64
		resp        *clientv3.MemberAddResponse
	)
	err = wait.ExponentialBackoff(etcdBackoff, func() (bool, error) {
		ctx, cancel := context.WithTimeout(context.Background(), etcdTimeout)
		defer cancel()
		if isLearner {
			// if learnerID is set, it means the etcd member is already added successfully.
			if learnerID == 0 {
				klog.V(1).Infof("[etcd] Adding etcd member as learner: %016x", peerAddrs)
				resp, err = cli.MemberAddAsLearner(ctx, []string{peerAddrs})
				if err != nil {
					lastError = err
					return false, nil
				}
				learnerID = resp.Member.ID
			}
			respMembers = resp.Members
			return true, nil
		}

		resp, err = cli.MemberAdd(ctx, []string{peerAddrs})
		if err == nil {
			respMembers = resp.Members
			return true, nil
		}

		// If the error indicates that the peer already exists, exit early. In this situation, resp is nil, so
		// call out to MemberList to fetch all the members before returning.
		if errors.Is(err, rpctypes.ErrPeerURLExist) {
			klog.V(5).Info("The peer URL for the added etcd member already exists. Fetching the existing etcd members")
			var listResp *clientv3.MemberListResponse
			listResp, err = cli.MemberList(ctx)
			if err == nil {
				respMembers = listResp.Members
				return true, nil
			}
		}

		klog.V(5).Infof("Failed to add etcd member: %v", err)
		lastError = err
		return false, nil
	})
	if err != nil {
		return nil, lastError
	}

	// Returns the updated list of etcd members
	ret := []Member{}
	for _, m := range respMembers {
		// If the peer address matches, this is the member we are adding.
		// Use the name we passed to the function.
		if peerAddrs == m.PeerURLs[0] {
			ret = append(ret, Member{Name: name, PeerURL: peerAddrs})
			continue
		}
		// Otherwise, we are processing other existing etcd members returned by AddMembers.
		memberName := m.Name
		// In some cases during concurrent join, some members can end up without a name.
		// Use the member ID as name for those.
		if len(memberName) == 0 {
			memberName = strconv.FormatUint(m.ID, 16)
		}
		ret = append(ret, Member{Name: memberName, PeerURL: m.PeerURLs[0]})
	}

	// Add the new member client address to the list of endpoints
	c.Endpoints = append(c.Endpoints, GetClientURLByIP(parsedPeerAddrs.Hostname()))

	return ret, nil
}

// isLearner returns true if the given member ID is a learner.
func (c *Client) isLearner(memberID uint64) (bool, error) {
	resp, err := c.listMembers()
	if err != nil {
		return false, err
	}

	for _, member := range resp.Members {
		if member.ID == memberID && member.IsLearner {
			return true, nil
		}
	}
	return false, nil
}

// MemberPromote promotes a member as a voting member. If the given member ID is already a voting member this method
// will return early and do nothing.
func (c *Client) MemberPromote(learnerID uint64) error {
	isLearner, err := c.isLearner(learnerID)
	if err != nil {
		return err
	}
	if !isLearner {
		klog.V(1).Infof("[etcd] Member %016x already promoted.", learnerID)
		return nil
	}

	klog.V(1).Infof("[etcd] Promoting a learner as a voting member: %016x", learnerID)
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   c.Endpoints,
		DialTimeout: etcdTimeout,
		DialOptions: []grpc.DialOption{
			grpc.WithBlock(), // block until the underlying connection is up
		},
		TLS: c.TLS,
	})
	if err != nil {
		return err
	}
	defer cli.Close()

	// TODO: warning logs from etcd client should be removed.
	// The warning logs are printed by etcd client code for several reasons, including
	// 1. can not promote yet(no synced)
	// 2. context deadline exceeded
	// 3. peer URLs already exists
	// Once the client provides a way to check if the etcd learner is ready to promote, the retry logic can be revisited.
	var (
		lastError error
	)
	err = wait.ExponentialBackoff(etcdBackoff, func() (bool, error) {
		ctx, cancel := context.WithTimeout(context.Background(), etcdTimeout)
		defer cancel()

		_, err = cli.MemberPromote(ctx, learnerID)
		if err == nil {
			klog.V(1).Infof("[etcd] The learner was promoted as a voting member: %016x", learnerID)
			return true, nil
		}
		klog.V(5).Infof("[etcd] Promoting the learner %016x failed: %v", learnerID, err)
		lastError = err
		return false, nil
	})
	if err != nil {
		return lastError
	}
	return nil
}

// CheckClusterHealth returns nil for status Up or error for status Down
func (c *Client) CheckClusterHealth() error {
	_, err := c.getClusterStatus()
	return err
}

// getClusterStatus returns nil for status Up (along with endpoint status response map) or error for status Down
func (c *Client) getClusterStatus() (map[string]*clientv3.StatusResponse, error) {
	clusterStatus := make(map[string]*clientv3.StatusResponse)
	for _, ep := range c.Endpoints {
		// Gets the member status
		var lastError error
		var resp *clientv3.StatusResponse
		err := wait.ExponentialBackoff(etcdBackoff, func() (bool, error) {
			cli, err := clientv3.New(clientv3.Config{
				Endpoints:   c.Endpoints,
				DialTimeout: etcdTimeout,
				DialOptions: []grpc.DialOption{
					grpc.WithBlock(), // block until the underlying connection is up
				},
				TLS: c.TLS,
			})
			if err != nil {
				lastError = err
				return false, nil
			}
			defer cli.Close()

			ctx, cancel := context.WithTimeout(context.Background(), etcdTimeout)
			resp, err = cli.Status(ctx, ep)
			cancel()
			if err == nil {
				return true, nil
			}
			klog.V(5).Infof("Failed to get etcd status for %s: %v", ep, err)
			lastError = err
			return false, nil
		})
		if err != nil {
			return nil, lastError
		}

		clusterStatus[ep] = resp
	}
	return clusterStatus, nil
}

// WaitForClusterAvailable returns true if all endpoints in the cluster are available after retry attempts, an error is returned otherwise
func (c *Client) WaitForClusterAvailable(retries int, retryInterval time.Duration) (bool, error) {
	for i := 0; i < retries; i++ {
		if i > 0 {
			klog.V(1).Infof("[etcd] Waiting %v until next retry\n", retryInterval)
			time.Sleep(retryInterval)
		}
		klog.V(2).Infof("[etcd] attempting to see if all cluster endpoints (%s) are available %d/%d", c.Endpoints, i+1, retries)
		_, err := c.getClusterStatus()
		if err != nil {
			switch err {
			case context.DeadlineExceeded:
				klog.V(1).Infof("[etcd] Attempt timed out")
			default:
				klog.V(1).Infof("[etcd] Attempt failed with error: %v\n", err)
			}
			continue
		}
		return true, nil
	}
	return false, errors.New("timeout waiting for etcd cluster to be available")
}

// GetClientURLByIP creates an HTTPS URL based on an IP address
// and the client listening port.
func GetClientURLByIP(ip string) string {
	return "https://" + net.JoinHostPort(ip, strconv.Itoa(2379))
}
