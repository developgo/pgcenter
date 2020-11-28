// Stuff related to network interfaces stats

package stat

import (
	"bufio"
	"fmt"
	"github.com/lesovsky/pgcenter/internal/postgres"
	"math"
	"os"
	"regexp"
)

// Netdev is the container for stats related to a single network interface
type Netdev struct {
	Ifname string /* interface name */
	Speed  uint32 /* interface network speed */
	Duplex uint8  /* interface duplex */
	// receive
	Rbytes      float64 /* total number of received bytes */
	Rpackets    float64 /* total number of received packets */
	Rerrs       float64 /* total number of receive errors */
	Rdrop       float64 /* total number of dropped packets */
	Rfifo       float64 /* total number of fifo buffers errors */
	Rframe      float64 /* total number of packet framing errors */
	Rcompressed float64 /* total number of received compressed packets */
	Rmulticast  float64 /* total number of received multicast packets */
	// transmit
	Tbytes      float64 /* total number of transmitted bytes */
	Tpackets    float64 /* total number of transmitted packets */
	Terrs       float64 /* total number of transmitted errors */
	Tdrop       float64 /* total number of dropped packets */
	Tfifo       float64 /* total number of fifo buffers errors */
	Tcolls      float64 /* total number of detected collisions */
	Tcarrier    float64 /* total number of carrier losses */
	Tcompressed float64 /* total number of received multicast packets */
	// enhanced
	Packets     float64 /* total number of received or transmitted packets */
	Raverage    float64 /* average size of received packets */
	Taverage    float64 /* average size of transmitted packets */
	Saturation  float64 /* saturation - the number of errors/second seen for the interface */
	Rutil       float64 /* percentage utilization for bytes received */
	Tutil       float64 /* percentage utilization for bytes transmitted */
	Utilization float64 /* percentage utilization of the interface */
	Uptime      float64 /* system uptime */
}

// Netdevs is the container for all stats of all network interfaces
type Netdevs []Netdev

const (
	// ProcNetdevFile is the location of network interfaces statistics in 'procfs' filesystem
	ProcNetdevFile = "/proc/net/dev"
	// pgProcLinkSettingsQuery quering network interfaces' details from Postgres instance
	pgProcLinkSettingsQuery = "SELECT speed::bigint * 1000000, duplex::bigint FROM pgcenter.get_netdev_link_settings($1);"
	// pgProcNetdevQuery queries network interfaces stats from Postgres instance
	pgProcNetdevQuery = "SELECT left(iface,-1),* FROM pgcenter.sys_proc_netdev ORDER BY iface"
)

func readNetdevs(db *postgres.DB, schemaExists bool) (Netdevs, error) {
	if db.Local {
		return readNetdevsLocal("/proc/net/dev")
	} else if schemaExists {
		return readNetdevsRemote(db)
	}

	return Netdevs{}, nil
}

func readNetdevsLocal(statfile string) (Netdevs, error) {
	var stat Netdevs
	f, err := os.Open(statfile)
	if err != nil {
		return stat, err
	}

	uptime, err := uptime()
	if err != nil {
		return nil, err
	}

	scanner := bufio.NewScanner(f)
	// skip header
	_ = scanner.Scan()
	_ = scanner.Scan()

	for scanner.Scan() {
		line := scanner.Text()

		var n = Netdev{}

		_, err = fmt.Sscanln(string(line),
			&n.Ifname,
			&n.Rbytes, &n.Rpackets, &n.Rerrs, &n.Rdrop, &n.Rfifo, &n.Rframe, &n.Rcompressed, &n.Rmulticast,
			&n.Tbytes, &n.Tpackets, &n.Terrs, &n.Tdrop, &n.Tfifo, &n.Tcolls, &n.Tcarrier, &n.Tcompressed)
		if err != nil {
			return nil, fmt.Errorf("%s bad content", statfile)
		}

		// skip pseudo block devices.
		re := regexp.MustCompile(`docker|virbr|veth`)
		if re.MatchString(n.Ifname) {
			continue
		}

		n.Saturation = n.Rerrs + n.Rdrop + n.Tdrop + n.Tfifo + n.Tcolls + n.Tcarrier

		n.Uptime = uptime

		// Get interface's speed and duplex, perhaps it's too expensive to poll interface in every execution of the function.
		n.Speed, n.Duplex, _ = GetLinkSettings(n.Ifname) /* use zeros if errors */

		stat = append(stat, n)
	}

	return stat, nil
}

func readNetdevsRemote(db *postgres.DB) (Netdevs, error) {
	var stat Netdevs
	var uptime float64
	db.QueryRow(pgProcUptimeQuery).Scan(&uptime)

	rows, err := db.Query(pgProcNetdevQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var dummy string
	for rows.Next() {
		var n = Netdev{}
		err := rows.Scan(&n.Ifname, &dummy,
			&n.Rbytes, &n.Rpackets, &n.Rerrs, &n.Rdrop, &n.Rfifo, &n.Rframe, &n.Rcompressed, &n.Rmulticast,
			&n.Tbytes, &n.Tpackets, &n.Terrs, &n.Tdrop, &n.Tfifo, &n.Tcolls, &n.Tcarrier, &n.Tcompressed)
		if err != nil {
			return nil, err
		}

		// skip pseudo block devices.
		re := regexp.MustCompile(`docker|virbr|veth`)
		if re.MatchString(n.Ifname) {
			continue
		}

		n.Uptime = uptime

		// Get interface's speed and duplex, perhaps it's too expensive to poll interface in every execution of the function.
		err = db.QueryRow(pgProcLinkSettingsQuery, n.Ifname).Scan(&n.Speed, &n.Duplex)
		if err != nil {
			return nil, err
		}

		stat = append(stat, n)
	}

	return stat, nil
}

func countNetdevsUsage(prev Netdevs, curr Netdevs, ticks float64) Netdevs {
	if len(curr) != len(prev) {
		// TODO: make possible to diff snapshots with different number of devices.
		return nil
	}

	stat := make([]Netdev, len(curr))

	for i := 0; i < len(curr); i++ {
		// Skip inactive interfaces
		if curr[i].Rpackets+curr[i].Tpackets == 0 {
			continue
		}

		itv := curr[i].Uptime - prev[i].Uptime
		stat[i].Ifname = curr[i].Ifname
		stat[i].Rbytes = sValue(prev[i].Rbytes, curr[i].Rbytes, itv, ticks)
		stat[i].Tbytes = sValue(prev[i].Tbytes, curr[i].Tbytes, itv, ticks)
		stat[i].Rpackets = sValue(prev[i].Rpackets, curr[i].Rpackets, itv, ticks)
		stat[i].Tpackets = sValue(prev[i].Tpackets, curr[i].Tpackets, itv, ticks)
		stat[i].Rerrs = sValue(prev[i].Rerrs, curr[i].Rerrs, itv, ticks)
		stat[i].Terrs = sValue(prev[i].Terrs, curr[i].Terrs, itv, ticks)
		stat[i].Tcolls = sValue(prev[i].Tcolls, curr[i].Tcolls, itv, ticks)
		stat[i].Saturation = sValue(prev[i].Saturation, curr[i].Saturation, itv, ticks)

		stat[i].Speed = curr[i].Speed
		stat[i].Duplex = curr[i].Duplex

		if stat[i].Rpackets > 0 {
			stat[i].Raverage = stat[i].Rbytes / stat[i].Rpackets
		} else {
			stat[i].Raverage = 0
		}
		if stat[i].Tpackets > 0 {
			stat[i].Taverage = stat[i].Tbytes / stat[i].Tpackets
		} else {
			stat[i].Taverage = 0
		}

		stat[i].Packets = curr[i].Rpackets + curr[i].Tpackets

		/* Calculate utilization */
		if curr[i].Speed > 0 {
			/* The following have a mysterious "800", it is 100 for the % conversion, and 8 for bytes2bits. */
			stat[i].Rutil = math.Min(stat[i].Rbytes*800/float64(curr[i].Speed), 100)
			stat[i].Tutil = math.Min(stat[i].Tbytes*800/float64(curr[i].Speed), 100)

			switch curr[i].Duplex {
			case duplexFull:
				stat[i].Utilization = math.Max(stat[i].Rutil, stat[i].Tutil)
			case duplexHalf:
				stat[i].Utilization = math.Min((stat[i].Rbytes+stat[i].Tbytes)*800/float64(curr[i].Speed), 100)
			case duplexUnknown:
			}
		} else {
			stat[i].Rutil, stat[i].Tutil, stat[i].Utilization = 0, 0, 0
		}
	}

	return stat
}