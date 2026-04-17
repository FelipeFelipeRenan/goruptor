resource "aws_db_instance" "goruptor_rds" {
  identifier             = "goruptor-db"
  engine                 = "postgres"
  engine_version         = "15"
  instance_class         = "db.t3.micro"
  allocated_storage      = 20

  # Credenciais do Banco
  db_name                = "exchange"
  username               = "goruptor"
  password               = "admin123"

  # Configurações de rede/segurança
  skip_final_snapshot    = true
  publicly_accessible    = true
}

# A AWS vai nos devolver qual foi a URL (Endpoint) gerada para o banco
output "rds_endpoint" {
  value = aws_db_instance.goruptor_rds.endpoint
}