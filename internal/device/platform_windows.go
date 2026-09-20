//go:build windows

package device

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"briard.io/tether/internal/family"
)

// The two Linux-only lines of the device layer, and Windows needs neither in this form. Its
// DCB carries the control-line state into CreateFile atomically, so INV 7 costs it nothing,
// and USB selective suspend is a driver property rather than a file to write.
//
// goneErrors stays empty because the library already answers the question here: an unplugged
// port surfaces as serial.PortNotFound, which device.go maps to ErrGone without help. On Linux
// the tty layer reports errnos instead, and that list is what translates them.
var goneErrors []error

func openControl(path string) (int, error) { return -1, nil }

func applyFlowControl(fd int, on bool) error {
	if on {
		return errors.New("RTS/CTS flow control is implemented for Linux only")
	}
	return nil
}

func closeControl(fd int) {}

func disableAutosuspend(path string) {}

// adviseUnstablePath has nothing to advise. A COM name is already per device *instance*, so a
// stick with a serial number keeps its number across replugs and there is no second, stabler
// name to prefer — where on Linux ttyUSB0 is exactly what a replug may change.
func adviseUnstablePath(path string) {}

// DEVPKEY_Device_BusReportedDeviceDesc is the adapter's own iProduct, cached by the hub driver
// at enumeration, measured. Everything easier to reach is the *driver's* name and so
// separates none of the sixteen rows sharing 10c4:ea60. Reading it touches the device, which
// matters: a CP210x is in selective suspend ten seconds after anything stops using it and
// answers no string descriptor request, so anything that probes iProduct live is unavailable at
// exactly the moment detection runs. This is not fetched again, so it still answers.
//
// DEVPKEY_Device_Parent is how the name is reached for an adapter whose COM port hangs off a
// child node rather than off the USB node itself — see nameFromParent.
var (
	devpkeyBusReportedDeviceDesc = windows.DEVPROPKEY{
		FmtID: windows.DEVPROPGUID{Data1: 0x540b947e, Data2: 0x8b40, Data3: 0x45bc,
			Data4: [8]byte{0xa8, 0xa2, 0x6a, 0x0b, 0x89, 0x4c, 0xbd, 0xa2}},
		PID: 4,
	}
	devpkeyParent = windows.DEVPROPKEY{
		FmtID: windows.DEVPROPGUID{Data1: 0x4340a6c5, Data2: 0x93fa, Data3: 0x4706,
			Data4: [8]byte{0x97, 0x2c, 0x7b, 0x64, 0x80, 0x08, 0xa5, 0xa7}},
		PID: 8,
	}
)

// devNode is one present device, as much of it as identification needs.
type devNode struct {
	instance string
	parent   string
	device   family.Device
	// port is the COM name, empty when the device has none — either because it is not a serial
	// device at all, or because no driver has given it one.
	port string
	// started says the devnode is actually running. ⚠️ It is not implied by having a port:
	// disabling a device leaves `Device Parameters\PortName` behind in the registry, so without
	// this tether offers a COM name that CreateFile then refuses, measured: an open attempt a
	// second, against a port that cannot open.
	started bool
}

// enumerate walks every device present on the machine.
//
// All classes and all enumerators, not the Ports class, and that is not thoroughness for its
// own sake: an adapter with no driver is **not in the Ports class**, because there is no INF yet
// to put it there, and naming that adapter is half of what this exists to do. Both halves are
// therefore one walk.
func enumerate() ([]devNode, error) {
	set, err := windows.SetupDiGetClassDevsEx(nil, "", 0,
		windows.DIGCF_ALLCLASSES|windows.DIGCF_PRESENT, 0, "")
	if err != nil {
		return nil, fmt.Errorf("looking for attached adapters: %w", err)
	}
	defer windows.SetupDiDestroyDeviceInfoList(set)

	var out []devNode
	for i := 0; ; i++ {
		data, err := windows.SetupDiEnumDeviceInfo(set, i)
		if err != nil {
			break // the walk ends with ERROR_NO_MORE_ITEMS, which is not a fault
		}
		instance, err := windows.SetupDiGetDeviceInstanceId(set, data)
		if err != nil {
			continue
		}
		vendor, product, ok := parseInstanceID(instance)
		if !ok {
			continue // not a USB device with ids in its name, which is most of a machine
		}

		n := devNode{
			instance: instance,
			parent:   stringProperty(set, data, &devpkeyParent),
			// ⚠️ Manufacturer is deliberately left empty, and that is the whole of the
			// platform difference. Windows has no bus-reported manufacturer:
			// DEVPKEY_Device_Manufacturer is the INF's provider, which is the driver's word
			// and not the device's. So the haystack here is the product alone, and Resolve
			// needs no Windows code to take it.
			device:  family.Device{Vendor: vendor, Product: product},
			port:    portName(set, data),
			started: started(data),
		}
		n.device.Name = stringProperty(set, data, &devpkeyBusReportedDeviceDesc)
		if n.device.Name == "" && n.port == "" {
			// A driverless adapter is the one case where the registry names are the device's own
			// word rather than an INF's, so it is the one case they may be fallen back to —
			// scoped to a portless node for exactly that reason. ⚠️ The two measured driverless
			// states do not agree on which field carries the name: with the driver package
			// removed, `DeviceDesc` has it and `FriendlyName` is empty, while on a never-driven
			// machine it is in `FriendlyName`. Both are tried, and in both states the
			// bus-reported property above answered anyway, so this is belt and braces rather
			// than the path.
			for _, prop := range []windows.SPDRP{windows.SPDRP_DEVICEDESC, windows.SPDRP_FRIENDLYNAME} {
				v, err := windows.SetupDiGetDeviceRegistryProperty(set, data, prop)
				if err != nil {
					continue
				}
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					n.device.Name = strings.TrimSpace(s)
					break
				}
			}
		}
		out = append(out, n)
	}
	return out, nil
}

func stringProperty(set windows.DevInfo, data *windows.DevInfoData, key *windows.DEVPROPKEY) string {
	v, err := windows.SetupDiGetDeviceProperty(set, data, key)
	if err != nil {
		return ""
	}
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// portName reads Device Parameters\PortName off the device instance key. Per *instance*, so a
// stick with a serial number keeps its COM number across replugs.
func portName(set windows.DevInfo, data *windows.DevInfoData) string {
	key, err := windows.SetupDiOpenDevRegKey(set, data,
		windows.DICS_FLAG_GLOBAL, 0, windows.DIREG_DEV, windows.KEY_READ)
	if err != nil {
		return ""
	}
	defer registry.Key(key).Close()
	name, _, err := registry.Key(key).GetStringValue("PortName")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(name)
}

// dnStarted marks a devnode the PnP manager has started. A device can be present, hold a COM
// name from the last time it ran, and not be running now.
const dnStarted = 0x00000008

func started(data *windows.DevInfoData) bool {
	var status, problem uint32
	if err := windows.CM_Get_DevNode_Status(&status, &problem, data.DevInst, 0); err != nil {
		// Nothing to go on: believe the port rather than hide a working adapter behind a
		// failed status call.
		return true
	}
	return status&dnStarted != 0
}

// nameFromParent fills in the bus-reported name of a node that does not carry one, from the node
// above it.
//
// The node holding the COM port is not always the USB node. A CP210x or a CDC adapter carries
// the port on the USB node itself and needs none of this; an FTDI adapter carries it on a child
// under its own enumerator — `FTDIBUS\VID_0403+PID_6015+…` — and the bus-reported description
// stays on the USB parent, where the hub driver put it. Without this hop a ConBee III would have
// ids and no name, and so would not resolve.
//
// ⚠️ Reasoned from the shape of the instance IDs, not measured: neither FTDI stick is in this
// lab. One hop, because the parent of a port node is the device.
func nameFromParent(nodes []devNode) {
	byInstance := make(map[string]int, len(nodes))
	for i, n := range nodes {
		byInstance[strings.ToUpper(n.instance)] = i
	}
	for i, n := range nodes {
		if n.device.Name != "" || n.parent == "" {
			continue
		}
		if j, ok := byInstance[strings.ToUpper(n.parent)]; ok {
			nodes[i].device.Name = nodes[j].device.Name
		}
	}
}

// adapters is the enumeration turned into the two answers the caller needs: what can be opened,
// and what is a coordinator that cannot be opened because it has no driver.
//
// Ports first and undriven nodes second, rather than one walk over both: a composite adapter can
// have a driver on one function and none on another, and taking them in one pass would let the
// undriven half claim the device and hide a port that works.
func adapters() (found []Adapter, unusable []devNode, err error) {
	nodes, err := enumerate()
	if err != nil {
		return nil, nil, err
	}
	nameFromParent(nodes)

	// Sorted so that "the first port of a composite adapter" is decided here and not by
	// enumeration order, which is the driver's business and not stable across boots.
	sort.SliceStable(nodes, func(i, j int) bool { return lessPort(nodes[i].port, nodes[j].port) })

	seen := make(map[string]bool)
	for _, n := range nodes {
		if n.port == "" || !n.started {
			continue
		}
		params, err := family.Resolve(n.device)
		if err != nil {
			continue // not a coordinator the table knows, which is nearly every serial port
		}
		if key := physical(n.instance, n.parent); !seen[key] {
			seen[key] = true
			name, _ := family.Identify(n.device)
			found = append(found, Adapter{Path: n.port, Device: n.device, Params: params, Name: name})
		}
	}
	// ⚠️ **Not keyed on a problem code**, which would be wrong — measured on a Windows VM. A
	// Sonoff whose driver package has been removed sits at `Status: OK`, `CM_PROB_NONE`, no COM
	// port: Windows reports no *problem* because nothing has been attempted, so a problem code
	// would make tether ignore the very stick it exists to name. The discriminator is the pass
	// above instead — a portless node whose physical device already produced a port is a parent
	// or a sibling function and is silently skipped, which is the case a problem code would be
	// reaching for.
	for _, n := range nodes {
		if n.port != "" && n.started {
			continue
		}
		if _, err := family.Resolve(n.device); err != nil {
			continue
		}
		if key := physical(n.instance, n.parent); !seen[key] {
			seen[key] = true
			unusable = append(unusable, n)
		}
	}
	return found, unusable, nil
}

// DescribeUSB identifies the adapter behind a COM name, for the family table to resolve.
func DescribeUSB(devPath string) (family.Device, error) {
	nodes, err := enumerate()
	if err != nil {
		return family.Device{}, err
	}
	nameFromParent(nodes)

	want := strings.ToUpper(strings.TrimPrefix(devPath, `\\.\`))
	for _, n := range nodes {
		if n.port != "" && strings.ToUpper(n.port) == want {
			return n.device, nil
		}
	}
	return family.Device{}, fmt.Errorf("no USB adapter is attached at %s", devPath)
}

// Detect finds which of the attached adapters are coordinators. It is the family table read
// backwards, exactly as on Linux — the same rows, the same descriptor matching — so a detected
// adapter and a configured one are the same thing downstream.
func Detect() ([]Adapter, error) {
	found, unusable, err := adapters()
	if err != nil {
		return nil, err
	}

	// An adapter Windows can see but not open is a diagnosis, not an absence, and it is worth
	// more than the refusal it replaces: tether names the stick in the operator's hand and says
	// what to do about it. It only becomes the answer when there is nothing to serve — another
	// adapter that works is served, and this is an advisory beside it.
	if len(found) == 0 && len(unusable) > 0 {
		return nil, errors.New(unusableReport(unusable))
	}
	for _, n := range unusable {
		sayOnce("device: %s is attached but %s; serving %s instead",
			n.device.Label(), whyUnusable(n), found[0].Path)
	}
	return found, nil
}

// sayOnce logs an advisory the first time it says something, and again only when what it says
// changes. ⚠️ The retry loop is why: `Detect` succeeding does not mean `open` stops calling it —
// measured on a Windows VM, where a port that enumerated but would not open printed the same
// advisory five times in four seconds. The caller's own rate limit covers the error it reports,
// not the advisories this layer adds underneath it.
var lastSaid struct {
	sync.Mutex
	text string
}

func sayOnce(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	lastSaid.Lock()
	defer lastSaid.Unlock()
	if line == lastSaid.text {
		return
	}
	lastSaid.text = line
	log.Print(line)
}

// whyUnusable is the difference between the two ways an adapter can be visible and unopenable,
// and they want opposite things from the reader: one is a driver to install, the other is a
// device somebody turned off.
func whyUnusable(n devNode) string {
	if n.port == "" {
		return "has no driver, so Windows gives it no COM port"
	}
	return fmt.Sprintf("Windows has not started it, so %s cannot be opened", n.port)
}

// unusableReport is the message a stranger with a new stick and a vanilla Windows actually meets:
// nothing is wrong with the adapter, Windows simply has no driver for it yet and so gives it no
// COM port at all. Spaced out over several lines because it is the one thing in a log that is
// otherwise a line a minute, and because the reader is being asked to go and do something.
//
// A *running* tether does not fetch a driver behind somebody's back — that is an install-time
// action, and `briard-tether install` is where it happens. What a running tether owes instead is
// to be diagnosable from outside and to hand a human at a command line an instruction they can
// follow, which is why the message quotes the very script the install verb runs. The driver half
// is included only when a driver is actually what is missing: telling somebody to fetch a driver
// for a device they disabled would be a worse answer than a flat refusal.
func unusableReport(unusable []devNode) string {
	var b strings.Builder
	b.WriteString("no usable COM port for an adapter Windows can see\n\n")
	ids := make([]string, 0, len(unusable))
	for _, n := range unusable {
		fmt.Fprintf(&b, "    %s (%s:%s) — %s\n", n.device.Label(),
			n.device.Vendor, n.device.Product, whyUnusable(n))
		if n.port == "" {
			ids = append(ids, hardwareID(n.device.Vendor, n.device.Product))
		}
	}
	if len(ids) == 0 {
		return b.String() + "\n    Enable it in Device Manager, and tether serves it the moment it starts"
	}
	return b.String() + driverInstructions(ids)
}

func hardwareID(vendor, product string) string {
	return fmt.Sprintf("VID_%s&PID_%s", strings.ToUpper(vendor), strings.ToUpper(product))
}

// driverScript is the PowerShell that asks Windows Update for those drivers and installs them.
// It is generated in one place and used twice — run by the install verb, and quoted verbatim in
// the message a running tether prints when it meets an adapter it cannot open — so the
// instruction and the implementation cannot drift into disagreeing.
//
// Every line of it is measured on a Windows VM, including the two that a reading would not have
// produced: **Download before Install**, without which Install fails every update with
// 0x80240022, and **ServerSelection = 2**, without which the search fails 0x8024802A.
//
// The filter is the point of generating this rather than printing something generic: Windows
// Update offers every pending driver, and tether is the only thing on the machine that knows
// which adapter is the one without a driver. `DriverHardwareID` answering through IDispatch is
// measured too — the count line below is what proved it, and is kept because a filter that
// matched nothing would otherwise install nothing and say nothing.
func driverScript(ids []string) string {
	quoted := make([]string, 0, len(ids))
	for _, id := range ids {
		quoted = append(quoted, fmt.Sprintf("%q", id))
	}
	return fmt.Sprintf(`$ids = @(%s)
$s = New-Object -ComObject Microsoft.Update.Session
$q = $s.CreateUpdateSearcher()
$q.ServerSelection = 2      # ssWindowsUpdate; anything else fails 0x8024802A
$q.Online = $true
$r = $q.Search("IsInstalled=0 and Type='Driver'")
$c = New-Object -ComObject Microsoft.Update.UpdateColl
foreach ($u in $r.Updates) {
    if ($ids | Where-Object { $u.DriverHardwareID -like "*$_*" }) {
        $u.AcceptEula(); $c.Add($u) | Out-Null
    }
}
if ($c.Count -eq 0) { throw "Windows Update offered no driver for $ids" }
$d = $s.CreateUpdateDownloader(); $d.Updates = $c; $d.Download() | Out-Null
$i = $s.CreateUpdateInstaller(); $i.Updates = $c
$res = $i.Install()
if ($res.ResultCode -ne 2) { throw "install returned $($res.ResultCode), hresult $($res.HResult)" }`,
		strings.Join(quoted, ","))
}

// driverInstructions is the half of that message a reader acts on, and it is a command rather
// than an assurance on purpose. "Windows Update will fetch it" is a bad instruction precisely
// when it is wrong: measured, a vanilla Windows waited twenty minutes for a driver that never
// arrived on its own, and `pnputil /scan-devices` — the obvious thing to reach for — does not
// ask Windows Update at all.
//
// **The install verb comes first because it is the shorter answer**: it fetches this and
// registers the service in one go. The script below is the same one it runs — generated
// by driverScript, not transcribed beside it — for somebody who wants the driver without the
// service, and it is filtered to this adapter rather than to every driver Windows Update is
// offering.
func driverInstructions(ids []string) string {
	var b strings.Builder
	b.WriteString(`
    Windows Update has the driver. It sometimes installs one unprompted, but do not wait on
    that — ask for it. The short way, from a terminal opened with Run as administrator:

        briard-tether install

    which fetches it and registers tether as a service. For the driver alone, in an elevated
    PowerShell — AcceptEula and the installer want SYSTEM, so if this says access denied, put
    it in a .ps1 and run that from a one-shot task:

        schtasks /create /tn tether-driver /ru SYSTEM /rl HIGHEST /sc once /st 00:00 /f /tr "powershell -NoProfile -File C:\path\to\driver.ps1"
        schtasks /run /tn tether-driver

`)
	for _, line := range strings.Split(driverScript(ids), "\n") {
		b.WriteString("        " + line + "\n")
	}
	b.WriteString(`
    pnputil /scan-devices does not do this; it never asks Windows Update.

    tether installs nothing itself`)
	return b.String()
}
