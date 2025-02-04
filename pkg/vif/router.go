package vif

import (
	"context"
	"net"

	"go.opentelemetry.io/otel"

	"github.com/datawire/dlib/dlog"
	"github.com/telepresenceio/telepresence/v2/pkg/routing"
	"github.com/telepresenceio/telepresence/v2/pkg/subnet"
	"github.com/telepresenceio/telepresence/v2/pkg/tracing"
)

type Router struct {
	// The vif device that packets will be routed through
	device Device
	// Original routes for subnets configured not to be proxied
	neverProxyRoutes []*routing.Route
	// A list of never proxied routes that have already been added to routing table
	staticOverrides []*routing.Route
	// The subnets that are currently being routed
	routedSubnets []*net.IPNet
}

func NewRouter(device Device) *Router {
	return &Router{device: device}
}

func (rt *Router) isNeverProxied(n *net.IPNet) bool {
	for _, r := range rt.neverProxyRoutes {
		if subnet.Equal(r.RoutedNet, n) {
			return true
		}
	}
	return false
}

func (rt *Router) GetRoutedSubnets() []*net.IPNet {
	return rt.routedSubnets
}

func (rt *Router) UpdateRoutes(ctx context.Context, plaseProxy, dontProxy []*net.IPNet) (err error) {
	for _, n := range dontProxy {
		if !rt.isNeverProxied(n) {
			r, err := routing.GetRoute(ctx, n)
			if err != nil {
				dlog.Error(ctx, err)
			} else {
				rt.neverProxyRoutes = append(rt.neverProxyRoutes, r)
			}
		}
	}

	// Remove all no longer desired subnets from the routedSubnets
	var removed []*net.IPNet
	rt.routedSubnets, removed = subnet.Partition(rt.routedSubnets, func(_ int, sn *net.IPNet) bool {
		for _, d := range plaseProxy {
			if subnet.Equal(sn, d) {
				return true
			}
		}
		return false
	})

	// Remove already routed subnets from the pleaseProxy list
	added, _ := subnet.Partition(plaseProxy, func(_ int, sn *net.IPNet) bool {
		for _, d := range rt.routedSubnets {
			if subnet.Equal(sn, d) {
				return false
			}
		}
		return true
	})

	// Add pleaseProxy subnets to the currently routed subnets
	rt.routedSubnets = append(rt.routedSubnets, added...)

	for _, sn := range removed {
		if err := rt.device.RemoveSubnet(ctx, sn); err != nil {
			dlog.Errorf(ctx, "failed to remove subnet %s: %v", sn, err)
		}
	}

	for _, sn := range added {
		if err := rt.device.AddSubnet(ctx, sn); err != nil {
			dlog.Errorf(ctx, "failed to add subnet %s: %v", sn, err)
		}
	}

	return rt.reconcileStaticOverrides(ctx)
}

func (rt *Router) reconcileStaticOverrides(ctx context.Context) (err error) {
	var desired []*routing.Route
	ctx, span := otel.GetTracerProvider().Tracer("").Start(ctx, "reconcileStaticRoutes")
	defer tracing.EndAndRecord(span, err)

	// We're not going to add static routes unless they're actually needed
	// (i.e. unless the existing CIDRs overlap with the never-proxy subnets)
	for _, r := range rt.neverProxyRoutes {
		for _, s := range rt.routedSubnets {
			if s.Contains(r.RoutedNet.IP) || r.Routes(s.IP) {
				desired = append(desired, r)
				break
			}
		}
	}

adding:
	for _, r := range desired {
		for _, c := range rt.staticOverrides {
			if subnet.Equal(r.RoutedNet, c.RoutedNet) {
				continue adding
			}
		}
		if err := r.AddStatic(ctx); err != nil {
			dlog.Errorf(ctx, "failed to add static route %s: %v", r, err)
		}
	}

removing:
	for _, c := range rt.staticOverrides {
		for _, r := range desired {
			if subnet.Equal(r.RoutedNet, c.RoutedNet) {
				continue removing
			}
		}
		if err := c.RemoveStatic(ctx); err != nil {
			dlog.Errorf(ctx, "failed to remove static route %s: %v", c, err)
		}
	}
	rt.staticOverrides = desired

	return nil
}

func (rt *Router) Close(ctx context.Context) {
	for _, sn := range rt.routedSubnets {
		if err := rt.device.RemoveSubnet(ctx, sn); err != nil {
			dlog.Errorf(ctx, "failed to remove subnet %s: %v", sn, err)
		}
	}
	for _, r := range rt.staticOverrides {
		if err := r.RemoveStatic(ctx); err != nil {
			dlog.Errorf(ctx, "failed to remove static route %s: %v", r, err)
		}
	}
}
