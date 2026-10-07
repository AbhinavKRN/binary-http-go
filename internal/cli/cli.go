package cli

import (
	"flag"
	"strconv"
	"time"
)

func Parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		if args[0] == "--" {
			return append(positional, args[1:]...), nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

type Seconds struct{ D *time.Duration }

func (s Seconds) String() string {
	if s.D == nil {
		return ""
	}
	return s.D.String()
}

func (s Seconds) Set(v string) error {
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		*s.D = time.Duration(f * float64(time.Second))
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return err
	}
	*s.D = d
	return nil
}
