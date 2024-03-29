package register // import "go.unistack.org/micro-router-register/v3"

import (
	"fmt"
	"sync"
	"time"

	"go.unistack.org/micro/v3/logger"
	"go.unistack.org/micro/v3/register"
	"go.unistack.org/micro/v3/router"
)

var (
	// RefreshInterval is the time at which we completely refresh the table
	RefreshInterval = time.Second * 120
	// PruneInterval is how often we prune the routing table
	PruneInterval = time.Second * 10
)

// rtr implements router interface
type rtr struct {
	sync.RWMutex
	table    *table
	exit     chan bool
	initChan chan bool
	opts     router.Options
	running  bool
}

// NewRouter creates new router and returns it
func NewRouter(opts ...router.Option) router.Router {
	options := router.NewOptions(opts...)

	// construct the router
	r := &rtr{
		opts:     options,
		initChan: make(chan bool),
	}

	// create the new table, passing the fetchRoute method in as a fallback if
	// the table doesn't contain the result for a query.
	r.table = newTable(r.lookup)

	// start the router
	r.start()
	return r
}

// Init initializes router with given options
func (r *rtr) Init(opts ...router.Option) error {
	r.Lock()
	for _, o := range opts {
		o(&r.opts)
	}
	r.Unlock()

	if r.opts.Register == nil {
		return fmt.Errorf("register not set")
	}

	// push a message to the init chan so the watchers
	// can reset in the case the register was changed
	go func() {
		r.initChan <- true
	}()

	return nil
}

// Options returns router options
func (r *rtr) Options() router.Options {
	r.RLock()
	defer r.RUnlock()

	options := r.opts

	return options
}

// Table returns routing table
func (r *rtr) Table() router.Table {
	r.Lock()
	defer r.Unlock()
	return r.table
}

func getDomain(srv *register.Service) string {
	// check the service metadata for domain
	// TODO: domain as Domain field in register?
	if srv.Metadata != nil && len(srv.Metadata["domain"]) > 0 {
		return srv.Metadata["domain"]
	} else if len(srv.Nodes) > 0 && srv.Nodes[0].Metadata != nil {
		return srv.Nodes[0].Metadata["domain"]
	}

	// otherwise return wildcard
	// TODO: return GlobalDomain or PublicDomain
	return register.DefaultDomain
}

// manageRoute applies action on a given route
func (r *rtr) manageRoute(route router.Route, action string) error {
	switch action {
	case "create":
		if err := r.table.Create(route); err != nil && err != router.ErrDuplicateRoute {
			return fmt.Errorf("failed adding route for service %s: %s", route.Service, err)
		}
	case "delete":
		if err := r.table.Delete(route); err != nil && err != router.ErrRouteNotFound {
			return fmt.Errorf("failed deleting route for service %s: %s", route.Service, err)
		}
	case "update":
		if err := r.table.Update(route); err != nil {
			return fmt.Errorf("failed updating route for service %s: %s", route.Service, err)
		}
	default:
		return fmt.Errorf("failed to manage route for service %s: unknown action %s", route.Service, action)
	}

	return nil
}

// createRoutes turns a service into a list routes basically converting nodes to routes
func (r *rtr) createRoutes(service *register.Service, network string) []router.Route {
	routes := make([]router.Route, 0, len(service.Nodes))

	for _, node := range service.Nodes {
		routes = append(routes, router.Route{
			Service:  service.Name,
			Address:  node.Address,
			Gateway:  "",
			Network:  network,
			Router:   r.opts.ID,
			Link:     router.DefaultLink,
			Metric:   router.DefaultLocalMetric,
			Metadata: node.Metadata,
		})
	}

	return routes
}

// manageServiceRoutes applies action to all routes of the service.
// It returns error of the action fails with error.
func (r *rtr) manageRoutes(service *register.Service, action, network string) error {
	// create a set of routes from the service
	routes := r.createRoutes(service, network)

	// if its a delete action and there's no nodes
	// it means we need to wipe out all the routes
	// for that service
	if action == "delete" && len(routes) == 0 {
		// delete the service entirely
		r.table.deleteService(service.Name, network)
		return nil
	}

	// create the routes in the table
	for _, route := range routes {
		if r.opts.Logger.V(logger.TraceLevel) {
			r.opts.Logger.Tracef(r.opts.Context, "Creating route %v domain: %v", route, network)
		}
		if err := r.manageRoute(route, action); err != nil {
			return err
		}
	}

	return nil
}

// manageRegisterRoutes applies action to all routes of each service found in the register.
// It returns error if either the services failed to be listed or the routing table action fails.
func (r *rtr) loadRoutes(reg register.Register) error {
	services, err := reg.ListServices(r.opts.Context, register.ListDomain(register.WildcardDomain))
	if err != nil {
		return fmt.Errorf("failed listing services: %v", err)
	}

	// add each service node as a separate route
	for _, service := range services {
		// get the services domain from metadata. Fallback to wildcard.
		domain := getDomain(service)

		// create the routes
		routes := r.createRoutes(service, domain)

		// if the routes exist save them
		if len(routes) > 0 {
			if r.opts.Logger.V(logger.TraceLevel) {
				r.opts.Logger.Tracef(r.opts.Context, "Creating routes for service %v domain: %v", service, domain)
			}
			for _, rt := range routes {
				err := r.table.Create(rt)

				// update the route to prevent it from expiring
				if err == router.ErrDuplicateRoute {
					err = r.table.Update(rt)
				}

				if err != nil {
					if r.opts.Logger.V(logger.ErrorLevel) {
						r.opts.Logger.Errorf(r.opts.Context, "Error creating route for service %v in domain %v: %v", service, domain, err)
					}
				}
			}
			continue
		}

		// otherwise get all the service info

		// get the service to retrieve all its info
		srvs, err := reg.LookupService(r.opts.Context, service.Name, register.LookupDomain(domain))
		if err != nil {
			if r.opts.Logger.V(logger.TraceLevel) {
				r.opts.Logger.Tracef(r.opts.Context, "Failed to get service %s domain: %s", service.Name, domain)
			}
			continue
		}

		// manage the routes for all returned services
		for _, srv := range srvs {
			routes := r.createRoutes(srv, domain)

			if len(routes) > 0 {
				if r.opts.Logger.V(logger.TraceLevel) {
					r.opts.Logger.Tracef(r.opts.Context, "Creating routes for service %v domain: %v", srv, domain)
				}
				for _, rt := range routes {
					err := r.table.Create(rt)

					// update the route to prevent it from expiring
					if err == router.ErrDuplicateRoute {
						err = r.table.Update(rt)
					}

					if err != nil {
						if r.opts.Logger.V(logger.ErrorLevel) {
							r.opts.Logger.Errorf(r.opts.Context, "Error creating route for service %v in domain %v: %v", service, domain, err)
						}
					}
				}
			}
		}
	}

	return nil
}

// lookup retrieves all the routes for a given service and creates them in the routing table
func (r *rtr) lookup(service string) ([]router.Route, error) {
	if r.opts.Logger.V(logger.TraceLevel) {
		r.opts.Logger.Tracef(r.opts.Context, "Fetching route for %s domain: %v", service, register.WildcardDomain)
	}

	services, err := r.opts.Register.LookupService(r.opts.Context, service, register.LookupDomain(register.WildcardDomain))
	if err == register.ErrNotFound {
		if r.opts.Logger.V(logger.TraceLevel) {
			r.opts.Logger.Tracef(r.opts.Context, "Failed to find route for %s", service)
		}
		return nil, router.ErrRouteNotFound
	} else if err != nil {
		if r.opts.Logger.V(logger.TraceLevel) {
			r.opts.Logger.Tracef(r.opts.Context, "Failed to find route for %s: %v", service, err)
		}
		return nil, fmt.Errorf("failed getting services: %v", err)
	}

	var routes []router.Route

	for _, srv := range services {
		domain := getDomain(srv)
		// TODO: should we continue to send the event indicating we created a route?
		// lookup is only called in the query path so probably not
		routes = append(routes, r.createRoutes(srv, domain)...)
	}

	return routes, nil
}

// watchRegister watches register and updates routing table based on the received events.
// It returns error if either the register watcher fails with error or if the routing table update fails.
func (r *rtr) watchRegister(w register.Watcher) error {
	exit := make(chan bool)

	defer func() {
		close(exit)
	}()

	go func() {
		defer w.Stop()

		select {
		case <-exit:
			return
		case <-r.initChan:
			return
		case <-r.exit:
			return
		}
	}()

	for {
		// get the next service
		res, err := w.Next()
		if err != nil {
			if err != register.ErrWatcherStopped {
				return err
			}
			break
		}

		// don't process nil entries
		if res.Service == nil {
			if logger.V(logger.TraceLevel) {
				logger.Trace(r.opts.Context, "Received a nil service")
			}
			continue
		}

		if r.opts.Logger.V(logger.TraceLevel) {
			r.opts.Logger.Tracef(r.opts.Context, "Router dealing with next route %s %+v\n", res.Action, res.Service)
		}

		// get the services domain from metadata. Fallback to wildcard.
		domain := getDomain(res.Service)

		// create/update or delete the route
		if err := r.manageRoutes(res.Service, res.Action, domain); err != nil {
			return err
		}
	}

	return nil
}

// start the router. Should be called under lock.
func (r *rtr) start() error {
	if r.running {
		return nil
	}

	if r.opts.Precache {
		// add all local service routes into the routing table
		if err := r.loadRoutes(r.opts.Register); err != nil {
			return fmt.Errorf("failed loading register routes: %s", err)
		}
	}

	// add default gateway into routing table
	if r.opts.Gateway != "" {
		// note, the only non-default value is the gateway
		route := router.Route{
			Service: "*",
			Address: "*",
			Gateway: r.opts.Gateway,
			Network: "*",
			Router:  r.opts.ID,
			Link:    router.DefaultLink,
			Metric:  router.DefaultLocalMetric,
		}
		if err := r.table.Create(route); err != nil {
			return fmt.Errorf("failed adding default gateway route: %s", err)
		}
	}

	// create error and exit channels
	r.exit = make(chan bool)

	// periodically refresh all the routes
	go func() {
		t1 := time.NewTicker(RefreshInterval)
		defer t1.Stop()

		t2 := time.NewTicker(PruneInterval)
		defer t2.Stop()

		for {
			select {
			case <-r.exit:
				return
			case <-t2.C:
				r.table.pruneRoutes(RefreshInterval)
			case <-t1.C:
				if err := r.loadRoutes(r.opts.Register); err != nil {
					if r.opts.Logger.V(logger.DebugLevel) {
						r.opts.Logger.Debugf(r.opts.Context, "failed refreshing register routes: %s", err)
					}
				}
			}
		}
	}()

	go func() {
		for {
			select {
			case <-r.exit:
				return
			default:
				if r.opts.Logger.V(logger.TraceLevel) {
					r.opts.Logger.Tracef(r.opts.Context, "Router starting register watch")
				}
				w, err := r.opts.Register.Watch(r.opts.Context, register.WatchDomain(register.WildcardDomain))
				if err != nil {
					if r.opts.Logger.V(logger.DebugLevel) {
						r.opts.Logger.Debug(r.opts.Context, "failed creating register watcher: %v", err)
					}
					time.Sleep(time.Second)
					continue
				}

				// watchRegister calls stop when it's done
				if err := r.watchRegister(w); err != nil {
					if r.opts.Logger.V(logger.DebugLevel) {
						r.opts.Logger.Debugf(r.opts.Context, "Error watching the register: %v", err)
					}
					time.Sleep(time.Second)
				}
			}
		}
	}()

	r.running = true

	return nil
}

// Lookup routes in the routing table
func (r *rtr) Lookup(q ...router.QueryOption) ([]router.Route, error) {
	return r.Table().Query(q...)
}

// Watch routes
func (r *rtr) Watch(opts ...router.WatchOption) (router.Watcher, error) {
	return r.table.Watch(opts...)
}

// Close the router
func (r *rtr) Close() error {
	r.Lock()
	defer r.Unlock()

	select {
	case <-r.exit:
		return nil
	default:
		if !r.running {
			return nil
		}
		close(r.exit)

	}

	r.running = false
	return nil
}

// String prints debugging information about router
func (r *rtr) String() string {
	return "register"
}

func (r *rtr) Name() string {
	return r.opts.Name
}
