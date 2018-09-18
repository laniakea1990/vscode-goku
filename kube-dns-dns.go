1. dns/cmd/kube-dns/dns.go
func main() {
	config := options.NewKubeDNSConfig()
	config.AddFlags(pflag.CommandLine)

	flag.InitFlags()
	// Convinces goflags that we have called Parse() to avoid noisy logs.
	// OSS Issue: kubernetes/kubernetes#17162.
	goflag.CommandLine.Parse([]string{})
	logs.InitLogs()
	defer logs.FlushLogs()

	version.PrintAndExitIfRequested()

	glog.V(0).Infof("version: %+v", version.VERSION)

	server := app.NewKubeDNSServerDefault(config) //server启动
	server.Run()
}

2. dns/cmd/kube-dns/app/server.go
func NewKubeDNSServerDefault(config *options.KubeDNSConfig) *KubeDNSServer {
	kubeClient, err := newKubeClient(config)
	if err != nil {
		glog.Fatalf("Failed to create a kubernetes client: %v", err)
	}

	var configSync dnsconfig.Sync
	switch {
	case config.ConfigMap != "" && config.ConfigDir != "":
		glog.Fatal("Cannot use both ConfigMap and ConfigDir")

	case config.ConfigMap != "":
		glog.V(0).Infof("Using configuration read from ConfigMap: %v:%v", config.ConfigMapNs, config.ConfigMap)
		configSync = dnsconfig.NewConfigMapSync(kubeClient, config.ConfigMapNs, config.ConfigMap)

	case config.ConfigDir != "":
		glog.V(0).Infof("Using configuration read from directory: %v with period %v", config.ConfigDir, config.ConfigPeriod)
		configSync = dnsconfig.NewFileSync(config.ConfigDir, config.ConfigPeriod)

	default:
		glog.V(0).Infof("ConfigMap and ConfigDir not configured, using values from command line flags")
		conf := dnsconfig.Config{Federations: config.Federations}
		if len(config.NameServers) > 0 {
			conf.UpstreamNameservers = strings.Split(config.NameServers, ",")
		}
		configSync = dnsconfig.NewNopSync(&conf)
	}

	return &KubeDNSServer{
		domain:         config.ClusterDomain,
		healthzPort:    config.HealthzPort,
		dnsBindAddress: config.DNSBindAddress,
		dnsPort:        config.DNSPort,
		nameServers:    config.NameServers,
		kd:             dns.NewKubeDNS(kubeClient, config.ClusterDomain, config.InitialSyncTimeout, configSync),
	}
}

3. dns/cmd/kube-dns/app/server.go
func (server *KubeDNSServer) Run() {
	pflag.VisitAll(func(flag *pflag.Flag) {
		glog.V(0).Infof("FLAG: --%s=%q", flag.Name, flag.Value)
	})
	setupSignalHandlers()
	server.startSkyDNSServer()
	server.kd.Start()
	server.setupHandlers()

	glog.V(0).Infof("Status HTTP port %v", server.healthzPort)
	if server.nameServers != "" {
		glog.V(0).Infof("Upstream nameservers: %s", server.nameServers)
	}
	glog.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", server.healthzPort), nil))
}

4. dns/pkg/dns/dns.go
func NewKubeDNS(client clientset.Interface, clusterDomain string, timeout time.Duration, configSync config.Sync) *KubeDNS {
	kd := &KubeDNS{
		kubeClient:          client,
		domain:              clusterDomain,
		cache:               treecache.NewTreeCache(),
		cacheLock:           sync.RWMutex{},
		nodesStore:          kcache.NewStore(kcache.MetaNamespaceKeyFunc),
		reverseRecordMap:    make(map[string]*skymsg.Service),
		clusterIPServiceMap: make(map[string]*v1.Service),
		domainPath:          util.ReverseArray(strings.Split(strings.TrimRight(clusterDomain, "."), ".")),
		initialSyncTimeout:  timeout,

		configLock: sync.RWMutex{},
		configSync: configSync,
	}

	kd.setEndpointsStore()
	kd.setServicesStore()

	return kd
}

5. dns/pkg/dns/dns.go
func (kd *KubeDNS) setServicesStore() {
	// Returns a cache.ListWatch that gets all changes to services.
	kd.servicesStore, kd.serviceController = kcache.NewInformer(
		kcache.NewListWatchFromClient(
			kd.kubeClient.Core().RESTClient(),
			"services",
			/* dns/vendor/k8s.io/client-go/pkg/api/v1/types.go
			const (
				// NamespaceDefault means the object is in the default namespace which is applied when not specified by clients
				NamespaceDefault string = "default"
				// NamespaceAll is the default argument to specify on a context when you want to list or filter resources across all namespaces
				NamespaceAll string = ""
			)
			*/
			v1.NamespaceAll,	//对k8s集群所有namespace的service进行watch
			fields.Everything()),
		&v1.Service{},
		resyncPeriod,
		kcache.ResourceEventHandlerFuncs{
			AddFunc:    kd.newService,
			DeleteFunc: kd.removeService,
			UpdateFunc: kd.updateService,
		},
	)
}

6. dns/vendor/k8s.io/client-go/tools/cache/controller.go
// NewInformer returns a Store and a controller for populating the store
// while also providing event notifications. You should only used the returned
// Store for Get/List operations; Add/Modify/Deletes will cause the event
// notifications to be faulty.
//
// Parameters:
//  * lw is list and watch functions for the source of the resource you want to
//    be informed of.
//  * objType is an object of the type that you expect to receive.
//  * resyncPeriod: if non-zero, will re-list this often (you will get OnUpdate
//    calls, even if nothing changed). Otherwise, re-list will be delayed as
//    long as possible (until the upstream source closes the watch or times out,
//    or you stop the controller).
//  * h is the object you want notifications sent to.
//
func NewInformer(
	lw ListerWatcher,
	objType runtime.Object,
	resyncPeriod time.Duration,
	h ResourceEventHandler,
) (Store, Controller) {
	// This will hold the client state, as we know it.
	clientState := NewStore(DeletionHandlingMetaNamespaceKeyFunc)

	// This will hold incoming changes. Note how we pass clientState in as a
	// KeyLister, that way resync operations will result in the correct set
	// of update/delete deltas.
	fifo := NewDeltaFIFO(MetaNamespaceKeyFunc, nil, clientState)

	cfg := &Config{
		Queue:            fifo,
		ListerWatcher:    lw,
		ObjectType:       objType,
		FullResyncPeriod: resyncPeriod,
		RetryOnError:     false,

		Process: func(obj interface{}) error {
			// from oldest to newest
			for _, d := range obj.(Deltas) {
				switch d.Type {
				case Sync, Added, Updated:
					if old, exists, err := clientState.Get(d.Object); err == nil && exists {
						if err := clientState.Update(d.Object); err != nil {
							return err
						}
						h.OnUpdate(old, d.Object)
					} else {
						if err := clientState.Add(d.Object); err != nil {
							return err
						}
						h.OnAdd(d.Object)
					}
				case Deleted:
					if err := clientState.Delete(d.Object); err != nil {
						return err
					}
					h.OnDelete(d.Object)
				}
			}
			return nil
		},
	}
	return clientState, New(cfg)
}

7. dns/vendor/k8s.io/client-go/tools/cache/listwatch.go
// NewListWatchFromClient creates a new ListWatch from the specified client, resource, namespace and field selector.
func NewListWatchFromClient(c Getter, resource string, namespace string, fieldSelector fields.Selector) *ListWatch {
	listFunc := func(options metav1.ListOptions) (runtime.Object, error) {
		return c.Get().
			Namespace(namespace).
			Resource(resource).
			VersionedParams(&options, metav1.ParameterCodec).
			FieldsSelectorParam(fieldSelector).
			Do().
			Get()
	}
	watchFunc := func(options metav1.ListOptions) (watch.Interface, error) {
		options.Watch = true
		return c.Get().
			Namespace(namespace).
			Resource(resource).
			VersionedParams(&options, metav1.ParameterCodec).
			FieldsSelectorParam(fieldSelector).
			Watch()
	}
	return &ListWatch{ListFunc: listFunc, WatchFunc: watchFunc}
}

7. dns/pkg/dns/dns.go
func (kd *KubeDNS) Start() {
	glog.V(2).Infof("Starting endpointsController")
	go kd.endpointsController.Run(wait.NeverStop)

	glog.V(2).Infof("Starting serviceController")
	go kd.serviceController.Run(wait.NeverStop)

	kd.startConfigMapSync()

	// Wait synchronously for the initial list operations to be
	// complete of endpoints and services from APIServer.
	kd.waitForResourceSyncedOrDie()
}

8.
// Run begins processing items, and will continue until a value is sent down stopCh.
// It's an error to call Run more than once.
// Run blocks; call via go.
func (c *controller) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	go func() {
		<-stopCh
		c.config.Queue.Close()
	}()
	r := NewReflector(
		c.config.ListerWatcher,
		c.config.ObjectType,
		c.config.Queue,
		c.config.FullResyncPeriod,
	)
	r.ShouldResync = c.config.ShouldResync
	r.clock = c.clock

	c.reflectorMutex.Lock()
	c.reflector = r
	c.reflectorMutex.Unlock()

	r.RunUntil(stopCh)

	wait.Until(c.processLoop, time.Second, stopCh)
}

9.dns/vendor/k8s.io/client-go/tools/cache/reflector.go
// RunUntil starts a watch and handles watch events. Will restart the watch if it is closed.
// RunUntil starts a goroutine and returns immediately. It will exit when stopCh is closed.
func (r *Reflector) RunUntil(stopCh <-chan struct{}) {
	glog.V(3).Infof("Starting reflector %v (%s) from %s", r.expectedType, r.resyncPeriod, r.name)
	go wait.Until(func() {
		if err := r.ListAndWatch(stopCh); err != nil {
			utilruntime.HandleError(err)
		}
	}, r.period, stopCh)
}