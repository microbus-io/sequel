package service

import "io/fs"

func NewService() *Service {
	return nil
}

func (svc *Service) SQLDataSourceName() (dsn string) {
	return ""
}

func (svc *Service) SetSQLDataSourceName(dsn string) {
}

func (svc *Service) Plane() string {
	return ""
}

func (svc *Service) Deployment() string {
	return ""
}

func (svc *Service) ResFS() fs.FS {
	return nil
}

func (svc *Service) Parallel(args ...any) error {
	return nil
}
