# A minimal Shiny app for trying out shinyctl.
#
# The page shows which R process is serving it. "Crash this app" makes that
# process exit with an error, and shinyctl starts a new one within a few
# seconds. The page reconnects to it by itself and shows the new process ID.

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
  # Lets an open page reconnect after the app restarts, instead of greying
  # out. "force" because TRUE only works behind Shiny Server or Posit
  # Connect; "force" also works under plain shiny::runApp.
  session$allowReconnect("force")

  # GREETING comes from the manifest's env field.
  output$greeting <- renderText(Sys.getenv("GREETING", "Hello"))

  output$process <- renderText(sprintf(
    "Served by R process %d, started at %s.",
    Sys.getpid(), format(started_at, "%H:%M:%S")
  ))

  # ignoreInit because a reconnecting page sends its inputs again, this
  # button's click count included, which would crash the new process too.
  observeEvent(input$crash, {
    message("Crash button clicked, exiting with status 1")
    quit(save = "no", status = 1)
  }, ignoreInit = TRUE)
}

shinyApp(ui, server)
