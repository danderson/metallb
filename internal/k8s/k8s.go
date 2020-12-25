package k8s // import "go.universe.tf/metallb/internal/k8s"

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"

	"go.universe.tf/metallb/internal/config"

	"github.com/go-kit/kit/log"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
)

// Client watches a Kubernetes cluster and translates events into
// Controller method calls.
type Client struct {
	logger log.Logger

	client *kubernetes.Clientset
	events record.EventRecorder
	queue  workqueue.RateLimitingInterface

	svcIndexer   cache.Indexer
	svcInformer  cache.Controller
	epIndexer    cache.Indexer
	epInformer   cache.Controller
	cmIndexer    cache.Indexer
	cmInformer   cache.Controller
	nodeIndexer  cache.Indexer
	nodeInformer cache.Controller

	syncFuncs []cache.InformerSynced

	serviceChanged func(log.Logger, string, *v1.Service, *v1.Endpoints) SyncState
	configChanged  func(log.Logger, *config.Config) SyncState
	nodeChanged    func(log.Logger, *v1.Node) SyncState
	synced         func(log.Logger)
}

// SyncState is the result of calling synchronization callbacks.
type SyncState int

const (
	// The update was processed successfully.
	SyncStateSuccess SyncState = iota
	// The update caused a transient error, the k8s client should
	// retry later.
	SyncStateError
	// The update was accepted, but requires reprocessing all watched
	// services.
	SyncStateReprocessAll
)

// Config specifies the configuration of the Kubernetes
// client/watcher.
type Config struct {
	ProcessName   string
	ConfigMapName string
	ConfigMapNS   string
	NodeName      string
	MetricsHost   string
	MetricsPort   int
	ReadEndpoints bool
	Logger        log.Logger
	Kubeconfig    string

	ServiceChanged func(log.Logger, string, *v1.Service, *v1.Endpoints) SyncState
	ConfigChanged  func(log.Logger, *config.Config) SyncState
	NodeChanged    func(log.Logger, *v1.Node) SyncState
	Synced         func(log.Logger)
}

type svcKey string
type cmKey string
type nodeKey string
type synced string

// New connects to masterAddr, using kubeconfig to authenticate.
//
// The client uses processName to identify itself to the cluster
// (e.g. when logging events).
func New(cfg *Config) (*Client, error) {
	var (
		k8sConfig *rest.Config
		err       error
	)

	if cfg.Kubeconfig == "" {
		// if the user didn't provide a config file, assume that we're
		// running inside k8s.
		k8sConfig, err = rest.InClusterConfig()
	} else {
		// the user provided a config file, so use that.  InClusterConfig
		// would also work in this case but it emits an annoying warning.
		k8sConfig, err = clientcmd.BuildConfigFromFlags("", cfg.Kubeconfig)
	}
	if err != nil {
		return nil, fmt.Errorf("building client config: %s", err)
	}
	clientset, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		return nil, fmt.Errorf("creating Kubernetes client: %s", err)
	}

	namespace := cfg.ConfigMapNS
	if namespace == "" {
		bs, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
		if err != nil {
			return nil, fmt.Errorf("getting namespace from pod service account data: %s", err)
		}
		namespace = string(bs)
	}

	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: v1core.New(clientset.CoreV1().RESTClient()).Events("")})
	recorder := broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: cfg.ProcessName})

	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	c := &Client{
		logger: cfg.Logger,
		client: clientset,
		events: recorder,
		queue:  queue,
	}

	if cfg.ServiceChanged != nil {
		svcHandlers := cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				key, err := cache.MetaNamespaceKeyFunc(obj)
				if err == nil {
					c.queue.Add(svcKey(key))
				}
			},
			UpdateFunc: func(old interface{}, new interface{}) {
				key, err := cache.MetaNamespaceKeyFunc(new)
				if err == nil {
					c.queue.Add(svcKey(key))
				}
			},
			DeleteFunc: func(obj interface{}) {
				key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
				if err == nil {
					c.queue.Add(svcKey(key))
				}
			},
		}
		svcWatcher := cache.NewListWatchFromClient(c.client.CoreV1().RESTClient(), "services", v1.NamespaceAll, fields.Everything())
		c.svcIndexer, c.svcInformer = cache.NewIndexerInformer(svcWatcher, &v1.Service{}, 0, svcHandlers, cache.Indexers{})

		c.serviceChanged = cfg.ServiceChanged
		c.syncFuncs = append(c.syncFuncs, c.svcInformer.HasSynced)

		if cfg.ReadEndpoints {
			epHandlers := cache.ResourceEventHandlerFuncs{
				AddFunc: func(obj interface{}) {
					key, err := cache.MetaNamespaceKeyFunc(obj)
					if err == nil {
						c.queue.Add(svcKey(key))
					}
				},
				UpdateFunc: func(old interface{}, new interface{}) {
					key, err := cache.MetaNamespaceKeyFunc(new)
					if err == nil {
						c.queue.Add(svcKey(key))
					}
				},
				DeleteFunc: func(obj interface{}) {
					key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
					if err == nil {
						c.queue.Add(svcKey(key))
					}
				},
			}
			epWatcher := cache.NewListWatchFromClient(c.client.CoreV1().RESTClient(), "endpoints", v1.NamespaceAll, fields.Everything())
			c.epIndexer, c.epInformer = cache.NewIndexerInformer(epWatcher, &v1.Endpoints{}, 0, epHandlers, cache.Indexers{})

			c.syncFuncs = append(c.syncFuncs, c.epInformer.HasSynced)
		}
	}

	if cfg.ConfigChanged != nil {
		cmHandlers := cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				key, err := cache.MetaNamespaceKeyFunc(obj)
				if err == nil {
					c.queue.Add(cmKey(key))
				}
			},
			UpdateFunc: func(old interface{}, new interface{}) {
				key, err := cache.MetaNamespaceKeyFunc(new)
				if err == nil {
					c.queue.Add(cmKey(key))
				}
			},
			DeleteFunc: func(obj interface{}) {
				key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
				if err == nil {
					c.queue.Add(cmKey(key))
				}
			},
		}
		cmWatcher := cache.NewListWatchFromClient(c.client.CoreV1().RESTClient(), "configmaps", namespace, fields.OneTermEqualSelector("metadata.name", cfg.ConfigMapName))
		c.cmIndexer, c.cmInformer = cache.NewIndexerInformer(cmWatcher, &v1.ConfigMap{}, 0, cmHandlers, cache.Indexers{})

		c.configChanged = cfg.ConfigChanged
		c.syncFuncs = append(c.syncFuncs, c.cmInformer.HasSynced)
	}

	if cfg.NodeChanged != nil {
		nodeHandlers := cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				key, err := cache.MetaNamespaceKeyFunc(obj)
				if err == nil {
					c.queue.Add(nodeKey(key))
				}
			},
			UpdateFunc: func(old interface{}, new interface{}) {
				key, err := cache.MetaNamespaceKeyFunc(new)
				if err == nil {
					c.queue.Add(nodeKey(key))
				}
			},
			DeleteFunc: func(obj interface{}) {
				key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
				if err == nil {
					c.queue.Add(nodeKey(key))
				}
			},
		}
		nodeWatcher := cache.NewListWatchFromClient(c.client.CoreV1().RESTClient(), "nodes", v1.NamespaceAll, fields.OneTermEqualSelector("metadata.name", cfg.NodeName))
		c.nodeIndexer, c.nodeInformer = cache.NewIndexerInformer(nodeWatcher, &v1.Node{}, 0, nodeHandlers, cache.Indexers{})

		c.nodeChanged = cfg.NodeChanged
		c.syncFuncs = append(c.syncFuncs, c.nodeInformer.HasSynced)
	}

	if cfg.Synced != nil {
		c.synced = cfg.Synced
	}

	http.Handle("/metrics", promhttp.Handler())
	go func() {
		http.ListenAndServe(fmt.Sprintf("%s:%d", cfg.MetricsHost, cfg.MetricsPort), nil)
	}()

	return c, nil
}

// PodIPs returns the IPs of all the pods matched by the labels string.
func (c *Client) PodsIPs(namespace, labels string) ([]string, error) {
	pl, err := c.client.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{LabelSelector: labels})
	if err != nil {
		return nil, err
	}
	iplist := []string{}
	for _, pod := range pl.Items {
		iplist = append(iplist, pod.Status.PodIP)
	}
	return iplist, nil
}

// Run watches for events on the Kubernetes cluster, and dispatches
// calls to the Controller.
func (c *Client) Run(stopCh <-chan struct{}) error {
	if c.svcInformer != nil {
		go c.svcInformer.Run(stopCh)
	}
	if c.epInformer != nil {
		go c.epInformer.Run(stopCh)
	}
	if c.cmInformer != nil {
		go c.cmInformer.Run(stopCh)
	}
	if c.nodeInformer != nil {
		go c.nodeInformer.Run(stopCh)
	}

	if !cache.WaitForCacheSync(stopCh, c.syncFuncs...) {
		return errors.New("timed out waiting for cache sync")
	}

	c.queue.Add(synced(""))

	if stopCh != nil {
		go func() {
			<-stopCh
			c.queue.ShutDown()
		}()
	}

	for {
		key, quit := c.queue.Get()
		if quit {
			return nil
		}
		updates.Inc()
		st := c.sync(key)
		switch st {
		case SyncStateSuccess:
			c.queue.Forget(key)
		case SyncStateError:
			updateErrors.Inc()
			c.queue.AddRateLimited(key)
		case SyncStateReprocessAll:
			c.queue.Forget(key)
			c.ForceSync()
		}
	}
}

// ForceSync reprocess all watched services
func (c *Client) ForceSync() {
	if c.svcIndexer != nil {
		for _, k := range c.svcIndexer.ListKeys() {
			c.queue.AddRateLimited(svcKey(k))
		}
	}
}

// Update writes svc back into the Kubernetes cluster. If successful,
// the updated Service is returned. Note that changes to svc.Status
// are not propagated, for that you need to call UpdateStatus.
func (c *Client) Update(svc *v1.Service) (*v1.Service, error) {
	return c.client.CoreV1().Services(svc.Namespace).Update(context.TODO(), svc, metav1.UpdateOptions{})
}

// UpdateStatus writes the protected "status" field of svc back into
// the Kubernetes cluster.
func (c *Client) UpdateStatus(svc *v1.Service) error {
	_, err := c.client.CoreV1().Services(svc.Namespace).UpdateStatus(context.TODO(), svc, metav1.UpdateOptions{})
	return err
}

// Infof logs an informational event about svc to the Kubernetes cluster.
func (c *Client) Infof(svc *v1.Service, kind, msg string, args ...interface{}) {
	c.events.Eventf(svc, v1.EventTypeNormal, kind, msg, args...)
}

// Errorf logs an error event about svc to the Kubernetes cluster.
func (c *Client) Errorf(svc *v1.Service, kind, msg string, args ...interface{}) {
	c.events.Eventf(svc, v1.EventTypeWarning, kind, msg, args...)
}

func (c *Client) sync(key interface{}) SyncState {
	defer c.queue.Done(key)

	switch k := key.(type) {
	case svcKey:
		l := log.With(c.logger, "service", string(k))
		svc, exists, err := c.svcIndexer.GetByKey(string(k))
		if err != nil {
			l.Log("op", "getService", "error", err, "msg", "failed to get service")
			return SyncStateError
		}
		if !exists {
			return c.serviceChanged(l, string(k), nil, nil)
		}

		var eps *v1.Endpoints
		if c.epIndexer != nil {
			epsIntf, exists, err := c.epIndexer.GetByKey(string(k))
			if err != nil {
				l.Log("op", "getEndpoints", "error", err, "msg", "failed to get endpoints")
				return SyncStateError
			}
			if !exists {
				return c.serviceChanged(l, string(k), nil, nil)
			}
			eps = epsIntf.(*v1.Endpoints)
		}

		return c.serviceChanged(l, string(k), svc.(*v1.Service), eps)

	case cmKey:
		l := log.With(c.logger, "configmap", string(k))
		cmi, exists, err := c.cmIndexer.GetByKey(string(k))
		if err != nil {
			l.Log("op", "getConfigMap", "error", err, "msg", "failed to get configmap")
			return SyncStateError
		}
		if !exists {
			configStale.Set(1)
			return c.configChanged(l, nil)
		}

		// Note that configs that we can read, but that fail parsing
		// or validation, result in a "synced" state, because the
		// config is not going to parse any better until the k8s
		// object changes to fix the issue.
		cm := cmi.(*v1.ConfigMap)
		cfg, err := config.Parse([]byte(cm.Data["config"]))
		if err != nil {
			l.Log("event", "configStale", "error", err, "msg", "config (re)load failed, config marked stale")
			configStale.Set(1)
			return SyncStateSuccess
		}

		st := c.configChanged(l, cfg)
		if st == SyncStateError {
			l.Log("event", "configStale", "error", err, "msg", "config (re)load failed, config marked stale")
			configStale.Set(1)
			return SyncStateSuccess
		}

		configLoaded.Set(1)
		configStale.Set(0)

		l.Log("event", "configLoaded", "msg", "config (re)loaded")
		return st

	case nodeKey:
		l := log.With(c.logger, "node", string(k))
		n, exists, err := c.nodeIndexer.GetByKey(string(k))
		if err != nil {
			l.Log("op", "getNode", "error", err, "msg", "failed to get node")
			return SyncStateError
		}
		if !exists {
			l.Log("op", "getNode", "error", "node doesn't exist in k8s, but I'm running on it!")
			return SyncStateError
		}
		node := n.(*v1.Node)
		return c.nodeChanged(c.logger, node)

	case synced:
		if c.synced != nil {
			c.synced(c.logger)
		}
		return SyncStateSuccess

	default:
		panic(fmt.Errorf("unknown key type for %#v (%T)", key, key))
	}
}
