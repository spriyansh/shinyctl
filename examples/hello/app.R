# A minimal Shiny app for trying out shinyctl.
#
# The page shows which R process is serving it. "Crash this app" makes that
# process exit with an error, and shinyctl starts a new one within a few
# seconds. Reload the page and the process ID has changed.

library(shiny)

# Top-level code runs once per R process, not once per browser session, so
# this is when the current process started.
started_at <- Sys.time()

ui <- fluidPage(
  titlePanel("shinyctl example"),
  h4(textOutput("greeting")),
  p(textOutput("process")),
  actionButton("crash", "Crash this app", class = "btn-danger")
)

server <- function(input, output, session) {
  # GREETING comes from the manifest's env field.
  output$greeting <- renderText(Sys.getenv("GREETING", "Hello"))

  output$process <- renderText(sprintf(
    "Served by R process %d, started at %s.",
    Sys.getpid(), format(started_at, "%H:%M:%S")
  ))

  observeEvent(input$crash, {
    message("Crash button clicked, exiting with status 1")
    quit(save = "no", status = 1)
  })
}

shinyApp(ui, server)
