# A minimal Shiny app for trying out shinyctl.
#
# The page shows which R process is serving it. "Crash this app" makes that
# process exit with an error, and shinyctl starts a new one within a few
# seconds. The page then reloads by itself and shows the new process ID.

library(shiny)

# Top-level code runs once per R process, not once per browser session, so
# this is when the current process started.
started_at <- Sys.time()

ui <- fluidPage(
  # Reloads the page once the app answers again after its R process died,
  # instead of leaving it greyed out. A reload, not Shiny's own reconnect:
  # with shiny 1.14, a page reconnected to a new process keeps showing its
  # old outputs, and its download links stop working. Skipped in the first
  # 3 seconds after a load, so an app that fails at start isn't reloaded
  # over and over.
  tags$script(HTML(
    "$(document).on('shiny:disconnected', function poll() {
       if (performance.now() < 3000) return;
       fetch(location.href, {cache: 'no-store'})
         .then(r => r.ok ? location.reload() : Promise.reject())
         .catch(() => setTimeout(poll, 500));
     });"
  )),
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
