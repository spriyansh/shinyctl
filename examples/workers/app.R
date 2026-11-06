# A Shiny app that hands its slow work to worker processes, for watching
# shinyctl treat those workers as part of the copy.
#
# At start the app launches two future workers (multisession: separate R
# processes). "Run a 15 second job" sums random numbers in a worker while the
# page stays responsive; the page shows the pid of the worker that ran it.
# "Run a job that holds 600 MB" allocates that much inside a worker and keeps
# it for a minute, so the copy's processes together go over the manifest's
# max-memory although the main R process stays small. Watch with
#
#   ps -A -o pid,ppid,pgid,rss,command | awk -v p=<main pid> '$1==p || $2==p || $3==p'
#
# where <main pid> is the PID column of `shinyctl status`: the workers share
# the main process's PGID. `shinyctl stop` ends all of them, and a copy that
# goes over max-memory is killed together with its workers.

library(shiny)
library(future)

workers <- as.integer(Sys.getenv("WORKERS", "2"))
plan(multisession, workers = workers)

# Top-level code runs once per R process, so this is the copy's own pid.
main_pid <- Sys.getpid()

# Each job runs in a worker; the worker's pid comes back with the result.
slow_job <- function(seconds) {
  deadline <- Sys.time() + seconds
  total <- 0
  while (Sys.time() < deadline) total <- total + sum(runif(1e5))
  list(pid = Sys.getpid(), total = total)
}
hungry_job <- function(mb) {
  held <- runif(mb * 125000)  # 8 bytes each: mb megabytes, written so they are really used
  Sys.sleep(60)
  list(pid = Sys.getpid(), mb = mb)
}

ui <- fluidPage(
  titlePanel("Workers"),
  p("Main R process pid: ", strong(main_pid), ", with ", workers, " workers."),
  actionButton("slow", "Run a 15 second job"),
  actionButton("hungry", "Run a job that holds 600 MB"),
  br(), br(),
  textOutput("status")
)

server <- function(input, output, session) {
  slow <- ExtendedTask$new(function() future(slow_job(15), seed = TRUE))
  hungry <- ExtendedTask$new(function() future(hungry_job(600), seed = TRUE))
  observeEvent(input$slow, slow$invoke())
  observeEvent(input$hungry, hungry$invoke())

  output$status <- renderText({
    lines <- character()
    if (slow$status() == "running") lines <- c(lines, "the 15 second job is running")
    if (slow$status() == "success") lines <- c(lines, sprintf("the 15 second job ran in worker pid %d", slow$result()$pid))
    if (hungry$status() == "running") lines <- c(lines, "a worker is holding 600 MB for a minute")
    if (hungry$status() == "success") lines <- c(lines, sprintf("worker pid %d held 600 MB and let it go", hungry$result()$pid))
    if (length(lines) == 0) "no job has run yet" else paste(lines, collapse = "; ")
  })
}

shinyApp(ui, server)
