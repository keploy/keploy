package secretscan

// The synthetic cases: configs that take the branches gitleaks' default rules
// and keploy's custom rules never take, and texts for them. Their findings in
// testdata/synthetic_golden.json are what gitleaks v8.28.0's own detector gave
// for these configs and texts. This file uses only gitleaks' config, so the
// same cases run against gitleaks to make the golden file.

import (
	"strings"

	"github.com/spf13/viper"
	"github.com/zricethezav/gitleaks/v8/config"
	"github.com/zricethezav/gitleaks/v8/regexp"
)

type synthCase struct {
	name  string
	toml  string               // a gitleaks config, read as gitleaks reads one
	cfg   func() config.Config // or a config built in code, which validation would refuse
	texts []string
}

func synthCaseConfig(c synthCase) (config.Config, error) {
	if c.cfg != nil {
		return c.cfg(), nil
	}
	v := viper.New()
	v.SetConfigType("toml")
	if err := v.ReadConfig(strings.NewReader(c.toml)); err != nil {
		return config.Config{}, err
	}
	var vc config.ViperConfig
	if err := v.Unmarshal(&vc); err != nil {
		return config.Config{}, err
	}
	return vc.Translate()
}

var synthCases = []synthCase{
	{
		name: "secret groups",
		toml: `
[[rules]]
id = "sg-third"
description = "secret group three"
regex = '''(sgk)=([a-z]+)-([0-9]+)'''
secretGroup = 3
tags = ["t1", "t2"]

[[rules]]
id = "sg-first-nonempty"
regex = '''fnk(x)?=([a-z0-9]+)'''

[[rules]]
id = "sg-one-group"
regex = '''ogk=([a-z0-9]+)'''

[[rules]]
id = "sg-none"
regex = '''nok=[a-z0-9]+'''
`,
		texts: []string{
			"sgk=abc-123",
			"fnk=abc123",
			"fnkx=abc123",
			"ogk=zz99",
			"nok=zz99",
			"fnk=aaaa",
			"x\ny\nfnk=abc123",
			"fnk=abc123\nx",
			"x\nfnk=abc123\ny",
			"fnk=ab1 fnk=cd2\nogk=ef3",
			"",
		},
	},
	{
		name: "a secret group past the last group",
		cfg: func() config.Config {
			return config.Config{Rules: map[string]config.Rule{
				"sg-past":      {RuleID: "sg-past", Regex: regexp.MustCompile(`(pgk)=([a-z]+)`), SecretGroup: 3},
				"sg-last":      {RuleID: "sg-last", Regex: regexp.MustCompile(`(lgk)=([a-z]+)`), SecretGroup: 2},
				"sg-no-groups": {RuleID: "sg-no-groups", Regex: regexp.MustCompile(`ngk=[a-z]+`), SecretGroup: 1},
			}}
		},
		texts: []string{"pgk=abc", "lgk=abc", "ngk=abc"},
	},
	{
		name: "a keyword in upper case",
		cfg: func() config.Config {
			return config.Config{
				Rules: map[string]config.Rule{
					"kw-upper": {RuleID: "kw-upper", Regex: regexp.MustCompile(`upk_[a-z0-9]{4}`), Keywords: []string{"UPK_"}},
				},
				Keywords: map[string]struct{}{"upk_": {}},
			}
		},
		texts: []string{"upk_abcd", "UPK_abcd"},
	},
	{
		name: "paths",
		toml: `
[[rules]]
id = "path-only-empty"
description = "path only, empty path"
path = '''^$'''

[[rules]]
id = "path-only-named"
path = '''\.env$'''

[[rules]]
id = "path-regex-empty"
path = '''^$'''
regex = '''prk=([a-z0-9]{6})'''

[[rules]]
id = "path-regex-named"
path = '''\.env$'''
regex = '''pnk=([a-z0-9]{6})'''
`,
		texts: []string{"prk=abc123 pnk=abc123", "nothing here", ""},
	},
	{
		name: "skip report",
		toml: `
[[rules]]
id = "skip-me"
regex = '''skk=([a-z0-9]+)'''
skipReport = true

[[rules]]
id = "report-me"
regex = '''rpk=([a-z0-9]+)'''
`,
		texts: []string{"skk=abc123 rpk=abc123"},
	},
	{
		name: "allowlists",
		toml: `
[[allowlists]]
description = "global stopword"
stopwords = ["globstop"]

[[allowlists]]
description = "global regex"
regexes = ['''^globre''']

[[allowlists]]
description = "global path, never a path here"
paths = ['''.*''']

[[rules]]
id = "al-plain"
regex = '''plk=([a-z0-9]+)'''

[[rules]]
id = "al-and-regex-stopword"
regex = '''aak=([a-z0-9]+)'''
[[rules.allowlists]]
condition = "AND"
regexes = ['''^allow''']
stopwords = ["word"]

[[rules]]
id = "al-and-stopword-only"
regex = '''aso=([a-z0-9]+)'''
[[rules.allowlists]]
condition = "AND"
stopwords = ["only"]

[[rules]]
id = "al-and-regex-only"
regex = '''aro=([a-z0-9]+)'''
[[rules.allowlists]]
condition = "AND"
regexes = ['''^rx''']

[[rules]]
id = "al-line-stopword"
regex = '''lsk=([a-z0-9]+)'''
[[rules.allowlists]]
regexTarget = "line"
stopwords = ["zzstop"]

[[rules]]
id = "al-and-path"
regex = '''apk=([a-z0-9]+)'''
[[rules.allowlists]]
condition = "AND"
paths = ['''.*''']
regexes = ['''.*''']

[[rules]]
id = "al-and-commit"
regex = '''ack=([a-z0-9]+)'''
[[rules.allowlists]]
condition = "AND"
commits = ["abcdef1"]
regexes = ['''.*''']

[[rules]]
id = "al-or-path"
regex = '''opk=([a-z0-9]+)'''
[[rules.allowlists]]
paths = ['''.*''']

[[rules]]
id = "al-target-match"
regex = '''rtm=([a-z0-9]+)'''
[[rules.allowlists]]
regexTarget = "match"
regexes = ['''^rtm=skip''']

[[rules]]
id = "al-target-line"
regex = '''rtl=([a-z0-9]+)'''
[[rules.allowlists]]
regexTarget = "line"
regexes = ['''#ok$''']

[[rules]]
id = "al-target-secret"
regex = '''rts=([a-z0-9]+)'''
[[rules.allowlists]]
regexes = ['''^skip''', '''^rts=''']
`,
		texts: []string{
			"plk=globstop1",
			"plk=globre1",
			"plk=keepme1",
			"aak=allowword",
			"aak=allowwrd",
			"aak=xallowword",
			"aso=onlyme",
			"aso=notme",
			"aro=rxme",
			"aro=notme",
			"lsk=abc zzstop",
			"lsk=zzstop1",
			"apk=abc123",
			"ack=abc123",
			"opk=abc123",
			"rtm=skipme",
			"rtm=keepme",
			"rtl=abc #ok",
			"x\nrtl=abc #ok\r",
			"x\nrtl=abc #ok\r\ny",
			"rtl=abc #no",
			"rts=skipme",
			"rts=keepme",
			"plk=abc123 # gitleaks:allow",
			"plk=abc123\n# gitleaks:allow",
			// Several matches of one rule, skipped ones before reported ones.
			// A match after the last newline has for its line the text
			// from its start, as in gitleaks, so the first two report
			// nothing and the next two report the matches on line 1.
			"plk=abc123 # gitleaks:allow\nplk=def456",
			"plk=abc123 # gitleaks:allow\r\nplk=def456 plk=ghi789",
			"plk=abc123 # gitleaks:allow\nplk=def456\nend",
			"plk=abc123 # gitleaks:allow\r\nplk=def456 plk=ghi789\r\nend",
			"plk=globstop1 plk=keepme2",
			"plk=globre1\nplk=keepme3",
			"rts=skipme rts=keepme",
			"rtm=skipme\nrtm=keepme",
			"rtl=abc #ok\nrtl=def #no",
			"aak=allowword aak=allowwrd",
			// More than five matches of one rule, one of them allowed.
			"plk=k01 plk=k02 plk=k03 plk=k04 plk=k05 plk=k06 plk=k07 plk=k08 plk=k09 plk=k10 plk=k11 plk=k12",
			"plk=k01\nplk=k02\nplk=k03 # gitleaks:allow\nplk=k04\nplk=k05\nplk=k06\nplk=k07\nplk=k08",
		},
	},
	{
		name: "entropy and newlines",
		toml: `
[[rules]]
id = "ent"
regex = '''ent=([a-z0-9]+)'''
entropy = 2.0

[[rules]]
id = "nl"
regex = '''nlk=[a-z]+\n'''

[[rules]]
id = "nl-lead"
regex = '''\nlnk=[a-z]+'''

[[rules]]
id = "multi-line"
regex = '''mlk=[a-z]+\n[a-z]+'''

[[rules]]
id = "ent-any"
regex = '''enu=(\S+)'''
entropy = 2.0
`,
		texts: []string{
			"ent=aaaa",
			"ent=abcd",
			"ent=abcde",
			// Low-entropy matches before reported ones, and more than five.
			"ent=aaaa ent=abcde",
			"ent=aaaa\nent=abcd\nent=abcde",
			"ent=aaaa ent=abcde ent=bbbb ent=bcdef ent=cccc ent=cdefg ent=dddd ent=defgh ent=eeee ent=efghi ent=ffff ent=fghij",
			// Non-ASCII secrets: gitleaks counts entropy over bytes, 1.99
			// bits here (at most 2.0, so skipped), 2.06 over runes.
			"enu=\"aaa:abé\"",
			"enu=pässwört",
			"nlk=abc\nnext",
			"x\nnlk=abc\n",
			"y\nlnk=abc",
			"mlk=abc\ndef",
			"x\nmlk=abc\ndef",
			"x\nmlk=abc\ndef\ny",
		},
	},
	{
		name: "keywords",
		toml: `
[[rules]]
id = "kw"
regex = '''(?i)kwz_[a-z0-9]{4}'''
keywords = ["KWZ_"]

[[rules]]
id = "kw-two"
regex = '''(?:qqa|qqb)=([a-z0-9]{4})'''
keywords = ["qqa", "qqb"]

[[rules]]
id = "kw-none"
regex = '''nkw=([a-z0-9]{4})'''
`,
		texts: []string{"kwz_abcd", "KWZ_ABCD", "qqb=abcd", "qqa=abcd qqb=efgh", "nkw=abcd", "kwz_ab"},
	},
	{
		name: "generic rules",
		toml: `
[[rules]]
id = "Generic-Thing"
regex = '''(?:gen|spc)=([a-z]+)'''

[[rules]]
id = "GENERIC-Other"
regex = '''oth=([a-z]+)'''

[[rules]]
id = "spc-thing"
regex = '''spc=([a-z]+[0-9]+)'''
`,
		texts: []string{
			"spc=abc123",
			"gen=abc\nspc=abc1",
			"gen=abc oth=abc",
			"gen=abc",
		},
	},
}
