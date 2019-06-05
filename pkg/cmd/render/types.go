package render

type FileUser struct {
	Id   *int   `yaml:"id"`
	Name string `yaml:"name"`
}

type FileGroup struct {
	Id   *int   `yaml:"id"`
	Name string `yaml:"name"`
}

type File struct {
	Filesystem string       `yaml:"filesystem"`
	Path       string       `yaml:"path"`
	User       *FileUser    `yaml:"user"`
	Group      *FileGroup   `yaml:"group"`
	Mode       *int         `yaml:"mode"`
	Contents   FileContents `yaml:"contents"`
	Overwrite  *bool        `yaml:"overwrite"`
	Append     bool         `yaml:"append"`
}

type FileContents struct {
	Remote Remote `yaml:"remote"`
	Inline string `yaml:"inline"`
	Local  string `yaml:"local"`
}

type Remote struct {
	Url          string       `yaml:"url"`
	Compression  string       `yaml:"compression"`
	Verification Verification `yaml:"verification"`
}

type Verification struct {
	Hash Hash `yaml:"hash"`
}

type Hash struct {
	Function string `yaml:"function"`
	Sum      string `yaml:"sum"`
}
