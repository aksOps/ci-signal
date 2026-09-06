package sample

type Service struct{}

func NewService() *Service { return &Service{} }

func (Service) Run() {}
