The lane's reachability probe with a valid token and Graphite's servers out of
reach — a proxy pointed at a closed port, so gt's own connection fails.

Recorded live (CCX_GT_RECORD_TOKEN): reaching the "cannot connect" branch takes
a token, since gt refuses for the missing one first. Pins gtProbeUnreadable, the
answer that leaves the verdict unknown rather than denied — a lane nobody could
confirm is not one to ride.
