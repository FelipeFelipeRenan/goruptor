# Postgres de desenvolvimento (emulado pelo MiniStack). A senha abaixo é apenas
# para o ambiente local; em qualquer ambiente real, use um secret manager.
resource "aws_db_instance" "goruptor_rds" {
  identifier        = "goruptor-db"
  engine            = "postgres"
  engine_version    = "15"
  instance_class    = "db.t3.micro"
  allocated_storage = 20

  db_name  = "exchange"
  username = "goruptor"
  password = "admin123"

  skip_final_snapshot = true
  publicly_accessible = true
}

# O endpoint criado pelo MiniStack é o que o servidor usa para falar com o Postgres
# (hoje fixo em cmd/goruptor-server/main.go).
output "rds_endpoint" {
  value       = aws_db_instance.goruptor_rds.endpoint
  description = "Endpoint do Postgres local"
}
