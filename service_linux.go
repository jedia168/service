package service

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"text/template"
	"time"

	"bitbucket.org/kardianos/osext"
)

const (
	initSystemV = initFlavor(iota)
	initUpstart
	initSystemd
)

func getFlavor() initFlavor {
	flavor := initSystemV
	if isSystemd() {
		flavor = initSystemd
	} else if isUpstart() {
		flavor = initUpstart
	}
	return flavor
}

func isUpstart() bool {
	if _, err := os.Stat("/sbin/upstart-udev-bridge"); err == nil {
		return true
	}
	return false
}

func isSystemd() bool {
	if _, err := os.Stat("/run/systemd/system"); err == nil {
		return true
	}
	return false
}

type linuxService struct {
	i Interface
	*Config

	interactive bool
}

var flavor = getFlavor()

type linuxSystem struct{}

func (ls linuxSystem) String() string {
	return fmt.Sprintf("Linux %s", flavor.String())
}

var system = linuxSystem{}

func newService(i Interface, c *Config) (Service, error) {
	s := &linuxService{
		i:      i,
		Config: c,
	}
	var err error
	s.interactive, err = isInteractive()

	return s, err
}

func (s *linuxService) String() string {
	if len(s.DisplayName) > 0 {
		return s.DisplayName
	}
	return s.Name
}

type initFlavor uint8

func (f initFlavor) String() string {
	switch f {
	case initSystemV:
		return "System-V"
	case initUpstart:
		return "Upstart"
	case initSystemd:
		return "systemd"
	default:
		return "unknown"
	}
}

func (f initFlavor) ConfigPath(name string) string {
	switch f {
	case initSystemd:
		return "/etc/systemd/system/" + name + ".service"
	case initSystemV:
		return "/etc/init.d/" + name
	case initUpstart:
		return "/etc/init/" + name + ".conf"
	default:
		return ""
	}
}

func (f initFlavor) GetTemplate() *template.Template {
	var templ string
	switch f {
	case initSystemd:
		templ = systemdScript
	case initSystemV:
		templ = systemVScript
	case initUpstart:
		templ = upstartScript
	}
	return template.Must(template.New(f.String() + "Script").Parse(templ))
}

func isInteractive() (bool, error) {
	// TODO: Is this true for user services?
	return os.Getppid() != 1, nil
}

func (s *linuxService) Interactive() bool {
	return s.interactive
}

func (s *linuxService) Install() error {
	confPath := flavor.ConfigPath(s.Name)
	_, err := os.Stat(confPath)
	if err == nil {
		return fmt.Errorf("Init already exists: %s", confPath)
	}

	f, err := os.Create(confPath)
	if err != nil {
		return err
	}
	defer f.Close()

	path, err := osext.Executable()
	if err != nil {
		return err
	}

	var to = &struct {
		Display     string
		Description string
		Path        string
	}{
		s.DisplayName,
		s.Description,
		path,
	}

	err = flavor.GetTemplate().Execute(f, to)
	if err != nil {
		return err
	}

	if flavor == initSystemV {
		if err = os.Chmod(confPath, 0755); err != nil {
			return err
		}
		for _, i := range [...]string{"2", "3", "4", "5"} {
			if err = os.Symlink(confPath, "/etc/rc"+i+".d/S50"+s.Name); err != nil {
				continue
			}
		}
		for _, i := range [...]string{"0", "1", "6"} {
			if err = os.Symlink(confPath, "/etc/rc"+i+".d/K02"+s.Name); err != nil {
				continue
			}
		}
	}

	if flavor == initSystemd {
		return exec.Command("systemctl", "daemon-reload").Run()
	}

	return nil
}

func (s *linuxService) Remove() error {
	if flavor == initSystemd {
		exec.Command("systemctl", "disable", s.Name+".service").Run()
	}
	if err := os.Remove(flavor.ConfigPath(s.Name)); err != nil {
		return err
	}
	return nil
}

func (s *linuxService) Logger() (Logger, error) {
	if s.interactive {
		return ConsoleLogger, nil
	}
	return s.SystemLogger()
}
func (s *linuxService) SystemLogger() (Logger, error) {
	return newSysLogger(s.Name)
}

func (s *linuxService) Run() (err error) {
	err = s.i.Start(s)
	if err != nil {
		return err
	}

	sigChan := make(chan os.Signal, 3)

	signal.Notify(sigChan, os.Interrupt, os.Kill)

	<-sigChan

	return s.i.Stop(s)
}

func (s *linuxService) Start() error {
	switch flavor {
	case initSystemd:
		return exec.Command("systemctl", "start", s.Name+".service").Run()
	case initUpstart:
		return exec.Command("initctl", "start", s.Name).Run()
	default:
		return exec.Command("service", s.Name, "start").Run()
	}
}

func (s *linuxService) Stop() error {
	switch flavor {
	case initSystemd:
		return exec.Command("systemctl", "stop", s.Name+".service").Run()
	case initUpstart:
		return exec.Command("initctl", "stop", s.Name).Run()
	default:
		return exec.Command("service", s.Name, "stop").Run()
	}
}

func (s *linuxService) Restart() error {
	err := s.Stop()
	if err != nil {
		return err
	}
	time.Sleep(50 * time.Millisecond)
	return s.Start()
}

const systemVScript = `#!/bin/sh
# For RedHat and cousins:
# chkconfig: - 99 01
# description: {{.Description}}
# processname: {{.Path}}

### BEGIN INIT INFO
# Provides:          {{.Path}}
# Required-Start:
# Required-Stop:
# Default-Start:     2 3 4 5
# Default-Stop:      0 1 6
# Short-Description: {{.Display}}
# Description:       {{.Description}}
### END INIT INFO

cmd="{{.Path}}"

name=$(basename $0)
pid_file="/var/run/$name.pid"
stdout_log="/var/log/$name.log"
stderr_log="/var/log/$name.err"

get_pid() {
    cat "$pid_file"
}

is_running() {
    [ -f "$pid_file" ] && ps $(get_pid) > /dev/null 2>&1
}

case "$1" in
    start)
        if is_running; then
            echo "Already started"
        else
            echo "Starting $name"
            $cmd >> "$stdout_log" 2>> "$stderr_log" &
            echo $! > "$pid_file"
            if ! is_running; then
                echo "Unable to start, see $stdout_log and $stderr_log"
                exit 1
            fi
        fi
    ;;
    stop)
        if is_running; then
            echo -n "Stopping $name.."
            kill $(get_pid)
            for i in {1..10}
            do
                if ! is_running; then
                    break
                fi
                echo -n "."
                sleep 1
            done
            echo
            if is_running; then
                echo "Not stopped; may still be shutting down or shutdown may have failed"
                exit 1
            else
                echo "Stopped"
                if [ -f "$pid_file" ]; then
                    rm "$pid_file"
                fi
            fi
        else
            echo "Not running"
        fi
    ;;
    restart)
        $0 stop
        if is_running; then
            echo "Unable to stop, will not attempt to start"
            exit 1
        fi
        $0 start
    ;;
    status)
        if is_running; then
            echo "Running"
        else
            echo "Stopped"
            exit 1
        fi
    ;;
    *)
    echo "Usage: $0 {start|stop|restart|status}"
    exit 1
    ;;
esac
exit 0`

const upstartScript = `# {{.Description}}

description     "{{.Display}}"

start on filesystem or runlevel [2345]
stop on runlevel [!2345]

#setuid username

respawn
respawn limit 10 5
umask 022

console none

pre-start script
    test -x {{.Path}} || { stop; exit 0; }
end script

# Start
exec {{.Path}}
`

const systemdScript = `[Unit]
Description={{.Description}}
ConditionFileIsExecutable={{.Path}}

[Service]
StartLimitInterval=5
StartLimitBurst=10
ExecStart={{.Path}}

[Install]
WantedBy=multi-user.target
`
